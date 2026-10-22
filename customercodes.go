package main

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Customer is who a fill is for. ID and Name are what gets logged and
// recorded against a dispense - never the login code itself, which
// exists only in the codes file (see ValidCodes).
type Customer struct {
	ID   string `json:"customerId"`
	Name string `json:"name"`
}

// CodeEntry is one admin-maintained login code -> customer mapping.
type CodeEntry struct {
	Code       string `json:"code"`
	CustomerID string `json:"customerId"`
	Name       string `json:"name"`
}

// codesFileHeader marks a codes file in the current three-field format.
// A file without it is the older "code,Full Name" format, converted on
// load (see reload).
const codesFileHeader = "# hydrogen-booster customer codes: code,customer ID,full name"

// validCustomerID allows the kind of IDs a billing system uses
// (e.g. "ACME-001") while keeping the codes file's comma-separated
// format unambiguous.
var validCustomerID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

// ValidCodes is the admin-maintained allow-list of customer login
// codes a fill may be started with, each mapped to a customer ID and
// name. Its file is the ONLY place login codes are stored: logs and
// dispense records carry the customer ID and name instead.
//
// The file is plain text so it's hand-editable on the device too:
//
//	# hydrogen-booster customer codes: code,customer ID,full name
//	5551234567,ACME-001,Acme Haulage
//
// "#"-prefixed lines are comments. Missing file = empty list, not an
// error, same as LoadConfigFile's "no file yet" handling.
type ValidCodes struct {
	mu    sync.RWMutex
	codes map[string]Customer // login code -> customer
	path  string
}

func LoadValidCodes(path string) (*ValidCodes, error) {
	v := &ValidCodes{path: path, codes: map[string]Customer{}}
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
			v.codes = map[string]Customer{}
			v.mu.Unlock()
			return nil
		}
		return fmt.Errorf("reading %s: %w", v.path, err)
	}
	text := string(data)
	legacy := !strings.Contains(text, codesFileHeader)

	var entries []CodeEntry
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var e CodeEntry
		if legacy {
			// Older "code,Full Name" format - the name may itself
			// contain commas, so split only once.
			parts := strings.SplitN(line, ",", 2)
			e.Code = strings.TrimSpace(parts[0])
			if len(parts) > 1 {
				e.Name = strings.TrimSpace(parts[1])
			}
		} else {
			e = parseCodeLine(line)
		}
		if e.Code != "" {
			entries = append(entries, e)
		}
	}

	if legacy && len(entries) > 0 {
		assignMissingCustomerIDs(entries)
		if err := v.SetAll(entries); err == nil {
			log.Printf("Converted %s to the code,customer ID,name format - generated customer IDs for %d customer(s); edit them on the admin screen to match your billing system", v.path, len(entries))
			return nil
		} else {
			// A hand-edited old file can hold entries SetAll rejects -
			// keep running with them in memory rather than refusing to
			// start; saving from the admin screen will surface the error.
			logErrorf("Could not convert %s to the code,customer ID,name format (%v) - using it unconverted until it's corrected and saved from the admin screen", v.path, err)
		}
	}

	codes := map[string]Customer{}
	for _, e := range entries {
		codes[e.Code] = Customer{ID: e.CustomerID, Name: e.Name}
	}
	v.mu.Lock()
	v.codes = codes
	v.mu.Unlock()
	return nil
}

// parseCodeLine splits a "code,customer ID,full name" line - the name
// is last, so it may contain commas.
func parseCodeLine(line string) CodeEntry {
	parts := strings.SplitN(line, ",", 3)
	e := CodeEntry{Code: strings.TrimSpace(parts[0])}
	if len(parts) > 1 {
		e.CustomerID = strings.TrimSpace(parts[1])
	}
	if len(parts) > 2 {
		e.Name = strings.TrimSpace(parts[2])
	}
	return e
}

// assignMissingCustomerIDs gives every entry without a customer ID a
// generated one (C0001, C0002, ... in code order), skipping any already
// in use.
func assignMissingCustomerIDs(entries []CodeEntry) {
	used := map[string]bool{}
	for _, e := range entries {
		if e.CustomerID != "" {
			used[strings.ToUpper(e.CustomerID)] = true
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Code < entries[j].Code })
	next := 1
	for i := range entries {
		if entries[i].CustomerID != "" {
			continue
		}
		for {
			id := fmt.Sprintf("C%04d", next)
			next++
			if !used[id] {
				entries[i].CustomerID = id
				used[id] = true
				break
			}
		}
	}
}

// Lookup returns the customer for a login code, and whether it's on
// the allow-list at all - the backing call for both Controller.Start's
// enforcement and the customer screen's welcome-message lookup.
func (v *ValidCodes) Lookup(code string) (Customer, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	c, ok := v.codes[code]
	return c, ok
}

// IsValid reports whether code is on the allow-list.
func (v *ValidCodes) IsValid(code string) bool {
	_, ok := v.Lookup(code)
	return ok
}

// List returns every entry, sorted by customer ID, for the admin
// screen.
func (v *ValidCodes) List() []CodeEntry {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]CodeEntry, 0, len(v.codes))
	for code, c := range v.codes {
		out = append(out, CodeEntry{Code: code, CustomerID: c.ID, Name: c.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CustomerID < out[j].CustomerID })
	return out
}

// SetAll validates and replaces the entire list, persisting it to file.
// Rejects (leaving the existing list untouched) a malformed code (the
// same 1-10 digit rule Controller.Start enforces), a missing or
// malformed customer ID, or a code or customer ID used twice. Blank
// rows (e.g. leftover from editing the admin textarea) are skipped.
func (v *ValidCodes) SetAll(entries []CodeEntry) error {
	codes := map[string]Customer{}
	ids := map[string]bool{}
	for _, e := range entries {
		code := strings.TrimSpace(e.Code)
		id := strings.TrimSpace(e.CustomerID)
		name := strings.TrimSpace(e.Name)
		if code == "" && id == "" && name == "" {
			continue
		}
		// Error messages identify the row by customer ID or name, never
		// by the code - they end up in the journal (see writeError).
		row := id
		if row == "" {
			row = name
		}
		if !isValidCustomerCode(code) {
			return fmt.Errorf("customer %q: login code must be 1-10 digits", row)
		}
		if _, dup := codes[code]; dup {
			return fmt.Errorf("customer %q: login code is already used by another customer", row)
		}
		if !validCustomerID.MatchString(id) {
			return fmt.Errorf("customer %q: customer ID is required - up to 32 letters, digits, '.', '_' or '-'", row)
		}
		if ids[strings.ToUpper(id)] {
			return fmt.Errorf("customer ID %q is used more than once", id)
		}
		ids[strings.ToUpper(id)] = true
		codes[code] = Customer{ID: id, Name: name}
	}

	list := make([]CodeEntry, 0, len(codes))
	for code, c := range codes {
		list = append(list, CodeEntry{Code: code, CustomerID: c.ID, Name: c.Name})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CustomerID < list[j].CustomerID })

	var buf strings.Builder
	buf.WriteString(codesFileHeader + "\n")
	for _, e := range list {
		fmt.Fprintf(&buf, "%s,%s,%s\n", e.Code, e.CustomerID, e.Name)
	}
	if err := os.WriteFile(v.path, []byte(buf.String()), 0600); err != nil {
		return fmt.Errorf("writing %s: %w", v.path, err)
	}

	v.mu.Lock()
	v.codes = codes
	v.mu.Unlock()
	return nil
}
