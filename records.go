package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// DispenseRecord is one completed fill: how much hydrogen went out, and
// who it was for, for reconciliation/billing against the customer code
// entered on the public screen.
type DispenseRecord struct {
	Timestamp       time.Time `json:"timestamp"`
	CustomerCode    string    `json:"customerCode"`
	DeliveredMassKg float64   `json:"deliveredMassKg"`
}

// DispenseLog is an append-only, file-backed record of completed fills -
// deliberately simple ("the database can be a file"): one JSON object
// per line, so appending never needs to read the existing file, and a
// partial/corrupt trailing line from a crash mid-write can't corrupt
// earlier records.
type DispenseLog struct {
	mu   sync.Mutex
	path string
}

func NewDispenseLog(path string) *DispenseLog {
	return &DispenseLog{path: path}
}

// Record appends one completed fill.
func (d *DispenseLog) Record(rec DispenseRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	f, err := os.OpenFile(d.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("opening %s: %w", d.path, err)
	}
	defer f.Close()

	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding record: %w", err)
	}
	line = append(line, '\n')
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("writing %s: %w", d.path, err)
	}
	return nil
}

// DrainAll returns every recorded fill and clears the file - the
// intended usage is a remote billing/accounting system periodically
// polling this to collect fills since it last asked, with the file
// cleared each time so nothing is double-counted. A malformed line
// (e.g. a torn write from a crash) is skipped rather than failing the
// whole drain.
func (d *DispenseLog) DrainAll() ([]DispenseRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	data, err := os.ReadFile(d.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", d.path, err)
	}

	var records []DispenseRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec DispenseRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		records = append(records, rec)
	}

	if err := os.WriteFile(d.path, nil, 0600); err != nil {
		return records, fmt.Errorf("clearing %s: %w", d.path, err)
	}
	return records, nil
}

// CodeEntry is one admin-maintained customer code -> name mapping.
type CodeEntry struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// ValidCodes is the admin-maintained allow-list of customer codes a fill
// may be started with, each mapped to the customer's name - shown as a
// welcome message on the customer screen once their code is recognised.
// Backed by a plain-text file (one "code,Full Name" pair per line,
// "#"-prefixed lines ignored) so it's hand-editable on the device too,
// matching config.json's philosophy. Missing file = empty list, not an
// error, same as LoadConfigFile's "no file yet" handling.
type ValidCodes struct {
	mu    sync.RWMutex
	codes map[string]string // code -> full name
	path  string
}

func LoadValidCodes(path string) (*ValidCodes, error) {
	v := &ValidCodes{path: path, codes: map[string]string{}}
	if err := v.reload(); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *ValidCodes) reload() error {
	data, err := os.ReadFile(v.path)
	if err != nil {
		if os.IsNotExist(err) {
			v.mu.Lock()
			v.codes = map[string]string{}
			v.mu.Unlock()
			return nil
		}
		return fmt.Errorf("reading %s: %w", v.path, err)
	}
	codes := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ",", 2)
		code := strings.TrimSpace(parts[0])
		if code == "" {
			continue
		}
		name := ""
		if len(parts) > 1 {
			name = strings.TrimSpace(parts[1])
		}
		codes[code] = name
	}
	v.mu.Lock()
	v.codes = codes
	v.mu.Unlock()
	return nil
}

// Lookup returns the customer's name for code and whether it's on the
// allow-list at all - the backing call for both Controller.Start's
// enforcement and the customer screen's welcome-message lookup.
func (v *ValidCodes) Lookup(code string) (name string, ok bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	name, ok = v.codes[code]
	return name, ok
}

// IsValid reports whether code is on the allow-list.
func (v *ValidCodes) IsValid(code string) bool {
	_, ok := v.Lookup(code)
	return ok
}

// List returns the current code/name pairs, sorted by code, for the
// admin screen.
func (v *ValidCodes) List() []CodeEntry {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]CodeEntry, 0, len(v.codes))
	for c, n := range v.codes {
		out = append(out, CodeEntry{Code: c, Name: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// SetAll validates and replaces the entire list, persisting it to file.
// Rejects (and leaves the existing list untouched) if any entry's code
// isn't valid, the same 1-10-digit rule Controller.Start enforces on the
// code itself. Blank rows (no code and no name - e.g. leftover from
// editing the admin textarea) are silently skipped.
func (v *ValidCodes) SetAll(entries []CodeEntry) error {
	seen := map[string]string{}
	for _, e := range entries {
		code := strings.TrimSpace(e.Code)
		name := strings.TrimSpace(e.Name)
		if code == "" && name == "" {
			continue
		}
		if !isValidCustomerCode(code) {
			return fmt.Errorf("%q is not a valid code (must be 1-10 digits)", code)
		}
		seen[code] = name
	}

	codes := make([]string, 0, len(seen))
	for c := range seen {
		codes = append(codes, c)
	}
	sort.Strings(codes)

	var buf strings.Builder
	for _, c := range codes {
		buf.WriteString(c)
		buf.WriteString(",")
		buf.WriteString(seen[c])
		buf.WriteString("\n")
	}
	if err := os.WriteFile(v.path, []byte(buf.String()), 0600); err != nil {
		return fmt.Errorf("writing %s: %w", v.path, err)
	}

	v.mu.Lock()
	v.codes = seen
	v.mu.Unlock()
	return nil
}
