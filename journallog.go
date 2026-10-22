package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// journalUnit is the systemd unit whose journal entries the log page
// shows - see systemd/hydrogen-booster.service.
const journalUnit = "hydrogen-booster.service"

// logRanges maps the log page's range choices to journalctl --since
// values; "" (all) passes no --since at all.
var logRanges = map[string]string{
	"":      "",
	"all":   "",
	"today": "today",
	"24h":   "-24h",
	"7d":    "-7d",
}

// handleLog streams this service's systemd journal entries, oldest
// first, as plain text - one entry per line:
//
//	<local time "2006-01-02 15:04:05">\t<syslog priority 0-7>\t<message>
//
// Query params: range (all|today|24h|7d, default all) and problems=1
// for warning priority and above only. Streamed straight from
// journalctl rather than buffered, so even a long history doesn't sit
// in memory; closing the page cancels the request, which kills
// journalctl. The service user needs to be in the systemd-journal
// group to read it (install.sh sets that up).
func (w *WebServer) handleLog(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "GET required", http.StatusMethodNotAllowed)
		return
	}
	since, ok := logRanges[r.URL.Query().Get("range")]
	if !ok {
		http.Error(rw, "range must be all, today, 24h or 7d", http.StatusBadRequest)
		return
	}
	path, err := exec.LookPath("journalctl")
	if err != nil {
		http.Error(rw, "journalctl is not available on this system - the log is only viewable on the device itself, where the service runs under systemd", http.StatusServiceUnavailable)
		return
	}

	args := []string{"-u", journalUnit, "--no-pager", "-o", "json", "--output-fields=MESSAGE,PRIORITY"}
	if since != "" {
		args = append(args, "--since", since)
	}
	if q := r.URL.Query().Get("problems"); q == "1" || q == "true" {
		args = append(args, "-p", "warning")
	}
	cmd := exec.CommandContext(r.Context(), path, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		http.Error(rw, fmt.Sprintf("starting journalctl: %v", err), http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	out := bufio.NewWriter(rw)
	n, scanErr := writeJournalLines(out, stdout)
	waitErr := cmd.Wait()

	// journalctl reports problems (most often missing permission to
	// read the system journal) on stderr - pass them on as a final
	// warning-priority line so the page shows why it's empty or short.
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		fmt.Fprintf(out, "%s\t4\tjournalctl: %s\n", time.Now().Format("2006-01-02 15:04:05"), strings.ReplaceAll(msg, "\n", " "))
	} else if waitErr != nil && r.Context().Err() == nil && n == 0 {
		fmt.Fprintf(out, "%s\t4\tjournalctl: %v\n", time.Now().Format("2006-01-02 15:04:05"), waitErr)
	}
	if scanErr != nil && r.Context().Err() == nil {
		fmt.Fprintf(out, "%s\t4\tlog stream: %v\n", time.Now().Format("2006-01-02 15:04:05"), scanErr)
	}
	_ = out.Flush()
}

// journalEntry is the subset of a journalctl -o json record handleLog
// uses. MESSAGE is normally a string, but journald encodes a message
// that isn't valid UTF-8 as an array of byte values instead.
type journalEntry struct {
	Realtime string          `json:"__REALTIME_TIMESTAMP"` // microseconds since the epoch, as a string
	Priority string          `json:"PRIORITY"`
	Message  json.RawMessage `json:"MESSAGE"`
}

// writeJournalLines converts journalctl's JSON-per-line output into
// handleLog's tab-separated lines, returning how many it wrote.
func writeJournalLines(out io.Writer, in io.Reader) (int, error) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	n := 0
	for sc.Scan() {
		var e journalEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		ts := ""
		if us, err := strconv.ParseInt(e.Realtime, 10, 64); err == nil {
			ts = time.UnixMicro(us).Format("2006-01-02 15:04:05")
		}
		prio := e.Priority
		if prio == "" {
			prio = "6"
		}
		msg := journalMessageText(e.Message)
		// Keep one entry per line - tabs/newlines inside a message
		// would break the page's line/field splitting.
		msg = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(msg)
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\n", ts, prio, msg); err != nil {
			return n, err
		}
		n++
	}
	return n, sc.Err()
}

func journalMessageText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var b []byte
	var ints []int
	if json.Unmarshal(raw, &ints) == nil {
		for _, v := range ints {
			b = append(b, byte(v))
		}
		return strings.ToValidUTF8(string(b), "?")
	}
	return ""
}
