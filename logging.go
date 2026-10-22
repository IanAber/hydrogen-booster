package main

import (
	"log"
	"os"
	"sync"
)

// Logging goes to stderr via the standard log package. Under systemd
// that stream lands in the journal (journalctl -u hydrogen-booster),
// and systemd sets JOURNAL_STREAM to say so - setupLogging then drops
// log's own timestamp (the journal records one) and the logErrorf/
// logWarnf helpers prefix lines with a syslog priority ("<3>", "<4>"),
// which journald strips and stores as the entry's priority, so
// `journalctl -u hydrogen-booster -p warning` shows just the problems.
// Run by hand (no JOURNAL_STREAM), output is plain timestamped lines.

var logToJournal bool

func setupLogging() {
	if os.Getenv("JOURNAL_STREAM") != "" {
		logToJournal = true
		log.SetFlags(0)
	}
}

func logWithPriority(priority, format string, args ...any) {
	if logToJournal {
		format = priority + format
	}
	log.Printf(format, args...)
}

// logErrorf logs a fault or failure (journal priority "err").
func logErrorf(format string, args ...any) { logWithPriority("<3>", format, args...) }

// logWarnf logs a rejected request or other non-fatal problem
// (journal priority "warning").
func logWarnf(format string, args ...any) { logWithPriority("<4>", format, args...) }

// errorLatch logs a recurring error only when it starts or its text
// changes, and logs once more when it clears - a module retrying every
// second against a broken bus would otherwise flood the journal with
// the same line all day.
type errorLatch struct {
	mu   sync.Mutex
	last string
}

// report logs err (prefixed with what) if it differs from the last one
// reported; a nil err logs recovery if an error was outstanding.
func (l *errorLatch) report(what string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		if l.last != "" {
			log.Printf("%s: recovered", what)
			l.last = ""
		}
		return
	}
	if msg := err.Error(); msg != l.last {
		logErrorf("%s: %s", what, msg)
		l.last = msg
	}
}
