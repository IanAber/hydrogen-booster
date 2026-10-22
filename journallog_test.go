package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWriteJournalLinesFormatsEntries(t *testing.T) {
	ts := time.Date(2026, 10, 22, 12, 7, 6, 0, time.Local)
	us := ts.UnixMicro()
	in := strings.Join([]string{
		`{"__REALTIME_TIMESTAMP":"` + itoa(us) + `","PRIORITY":"3","MESSAGE":"FAULT: Hydrogen detected - emergency stop"}`,
		`{"__REALTIME_TIMESTAMP":"` + itoa(us) + `","PRIORITY":"6","MESSAGE":"two\nlines\twith tab"}`,
		`{"__REALTIME_TIMESTAMP":"` + itoa(us) + `","PRIORITY":"4","MESSAGE":[104,105,255]}`,
		`not json`,
	}, "\n")
	var out bytes.Buffer
	n, err := writeJournalLines(&out, strings.NewReader(in))
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	want := "2026-10-22 12:07:06\t3\tFAULT: Hydrogen detected - emergency stop\n" +
		"2026-10-22 12:07:06\t6\ttwo lines with tab\n" +
		"2026-10-22 12:07:06\t4\thi?\n"
	if out.String() != want {
		t.Errorf("got:\n%q\nwant:\n%q", out.String(), want)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// fakeJournalctl puts a journalctl script on PATH that records its
// arguments and prints one entry.
func fakeJournalctl(t *testing.T) (argsFile string) {
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\n" +
		`echo '{"__REALTIME_TIMESTAMP":"1792685226000000","PRIORITY":"4","MESSAGE":"Rejected POST /api/start: job already running"}'` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

func TestHandleLogRunsJournalctlForThisUnit(t *testing.T) {
	argsFile := fakeJournalctl(t)
	w := &WebServer{}
	rec := httptest.NewRecorder()
	w.handleLog(rec, httptest.NewRequest(http.MethodGet, "/api/log?range=24h&problems=1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\t4\tRejected POST /api/start: job already running\n") {
		t.Errorf("body = %q", rec.Body.String())
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"-u hydrogen-booster.service", "-o json", "--since -24h", "-p warning"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("journalctl args %q missing %q", args, want)
		}
	}
}

func TestHandleLogRejectsUnknownRange(t *testing.T) {
	fakeJournalctl(t)
	rec := httptest.NewRecorder()
	(&WebServer{}).handleLog(rec, httptest.NewRequest(http.MethodGet, "/api/log?range=forever", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleLogWithoutJournalctl(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	rec := httptest.NewRecorder()
	(&WebServer{}).handleLog(rec, httptest.NewRequest(http.MethodGet, "/api/log", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}
