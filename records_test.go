package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestDispenseStoreRecordAndQueryByRange(t *testing.T) {
	d := newTestDispenseStore(t)
	day := func(n int) time.Time { return time.Date(2026, 10, n, 12, 0, 0, 0, time.UTC) }
	for _, r := range []DispenseRecord{
		{Timestamp: day(20), CustomerID: "ACME-001", CustomerName: "Acme Haulage", DeliveredMassKg: 1.5},
		{Timestamp: day(21), CustomerID: "ACME-002", CustomerName: "Bob Smith", DeliveredMassKg: 2.25},
		{Timestamp: day(22), CustomerID: "ACME-001", CustomerName: "Acme Haulage", DeliveredMassKg: 3},
	} {
		if err := d.Record(r); err != nil {
			t.Fatal(err)
		}
	}

	all, err := d.Query(time.Time{}, time.Time{}, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("all: %d records, err %v", len(all), err)
	}
	if all[0].CustomerID != "ACME-001" || all[0].DeliveredMassKg != 1.5 || !all[0].Timestamp.Equal(day(20)) {
		t.Errorf("all[0] = %+v", all[0])
	}

	// from inclusive, to exclusive.
	mid, _ := d.Query(day(21), day(22), "")
	if len(mid) != 1 || mid[0].CustomerID != "ACME-002" {
		t.Errorf("range query = %+v", mid)
	}

	acme, _ := d.Query(time.Time{}, time.Time{}, "ACME-001")
	if len(acme) != 2 {
		t.Errorf("customer query = %+v", acme)
	}

	// Reading doesn't remove anything.
	again, _ := d.Query(time.Time{}, time.Time{}, "")
	if len(again) != 3 {
		t.Errorf("records should be kept after reading, got %d", len(again))
	}
}

func TestImportLegacyDispenseLogTranslatesCodesAndDeletesFile(t *testing.T) {
	dir := t.TempDir()
	codes, err := LoadValidCodes(dir + "/valid-codes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := codes.SetAll([]CodeEntry{{Code: "5551234567", CustomerID: "ACME-001", Name: "Acme Haulage"}}); err != nil {
		t.Fatal(err)
	}
	legacy := dir + "/dispense-log.jsonl"
	lines := `{"timestamp":"2026-10-20T12:00:00Z","customerCode":"5551234567","deliveredMassKg":1.5}
not json
{"timestamp":"2026-10-21T12:00:00Z","customerCode":"9999999999","deliveredMassKg":2}
{"timestamp":"2026-10-22T12:00:00Z","customerCode":"","deliveredMassKg":3}
`
	if err := os.WriteFile(legacy, []byte(lines), 0600); err != nil {
		t.Fatal(err)
	}

	d := newTestDispenseStore(t)
	n, err := d.ImportLegacyDispenseLog(legacy, codes)
	if err != nil || n != 3 {
		t.Fatalf("imported %d, err %v", n, err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("the old file holds login codes and must be deleted after import")
	}
	recs, _ := d.Query(time.Time{}, time.Time{}, "")
	if len(recs) != 3 {
		t.Fatalf("records = %+v", recs)
	}
	if recs[0].CustomerID != "ACME-001" || recs[0].CustomerName != "Acme Haulage" {
		t.Errorf("known code: %+v", recs[0])
	}
	if recs[1].CustomerID != "UNKNOWN" {
		t.Errorf("code no longer listed: %+v, want UNKNOWN", recs[1])
	}
	if recs[2].CustomerID != "" {
		t.Errorf("no code: %+v, want empty customer", recs[2])
	}

	// Missing file: nothing to do.
	if n, err := d.ImportLegacyDispenseLog(legacy, codes); err != nil || n != 0 {
		t.Errorf("second import: n=%d err=%v", n, err)
	}
}

func TestValidCodesLookupAndPersist(t *testing.T) {
	path := t.TempDir() + "/valid-codes.txt"
	v, err := LoadValidCodes(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.SetAll([]CodeEntry{
		{Code: "5551234567", CustomerID: "ACME-001", Name: "Acme Haulage, Ltd"},
		{Code: "4321", CustomerID: "ACME-002", Name: "Bob Smith"},
	}); err != nil {
		t.Fatal(err)
	}

	v2, err := LoadValidCodes(path)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := v2.Lookup("5551234567")
	if !ok || c.ID != "ACME-001" || c.Name != "Acme Haulage, Ltd" {
		t.Errorf("Lookup = %+v, %v", c, ok)
	}
	if v2.IsValid("0000000000") {
		t.Error("expected an unlisted code to be invalid")
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), codesFileHeader+"\n") {
		t.Errorf("file should start with the format header:\n%s", data)
	}
}

func TestValidCodesConvertsLegacyFile(t *testing.T) {
	path := t.TempDir() + "/valid-codes.txt"
	if err := os.WriteFile(path, []byte("5559876543,John Smith\n# a comment\n5551234567,Doe, Jane\n\n"), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := LoadValidCodes(path)
	if err != nil {
		t.Fatal(err)
	}
	// IDs are generated in code order; names with commas survive.
	if c, _ := v.Lookup("5551234567"); c.ID != "C0001" || c.Name != "Doe, Jane" {
		t.Errorf("5551234567 -> %+v", c)
	}
	if c, _ := v.Lookup("5559876543"); c.ID != "C0002" || c.Name != "John Smith" {
		t.Errorf("5559876543 -> %+v", c)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), codesFileHeader) || !strings.Contains(string(data), "5551234567,C0001,Doe, Jane") {
		t.Errorf("file not rewritten in the new format:\n%s", data)
	}
}

func TestValidCodesMissingFileIsEmptyNotError(t *testing.T) {
	v, err := LoadValidCodes(t.TempDir() + "/does-not-exist.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.List()) != 0 {
		t.Errorf("expected an empty list, got %v", v.List())
	}
}

func TestValidCodesSetAllRejectsBadEntriesWithoutRevealingCode(t *testing.T) {
	v, err := LoadValidCodes(t.TempDir() + "/valid-codes.txt")
	if err != nil {
		t.Fatal(err)
	}
	good := CodeEntry{Code: "1234567890", CustomerID: "OK-1", Name: "OK"}
	if err := v.SetAll([]CodeEntry{good}); err != nil {
		t.Fatal(err)
	}
	for name, entries := range map[string][]CodeEntry{
		"non-digit code": {{Code: "12a4", CustomerID: "X-1", Name: "Bad"}},
		"missing ID":     {{Code: "5551234567", Name: "No ID"}},
		"ID with comma":  {{Code: "5551234567", CustomerID: "A,B", Name: "Comma"}},
		"duplicate ID":   {{Code: "5551234567", CustomerID: "dup", Name: "A"}, {Code: "5559876543", CustomerID: "DUP", Name: "B"}},
		"duplicate code": {{Code: "5551234567", CustomerID: "A-1", Name: "A"}, {Code: "5551234567", CustomerID: "B-1", Name: "B"}},
	} {
		err := v.SetAll(entries)
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		for _, e := range entries {
			if e.Code != "" && strings.Contains(err.Error(), e.Code) {
				t.Errorf("%s: error %q reveals the login code", name, err)
			}
		}
	}
	// A rejected SetAll leaves the earlier list intact.
	if !v.IsValid(good.Code) {
		t.Error("expected the previous list to survive a rejected SetAll")
	}
}

func TestValidCodesSetAllSkipsBlankRows(t *testing.T) {
	v, err := LoadValidCodes(t.TempDir() + "/valid-codes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.SetAll([]CodeEntry{{Code: "1234567890", CustomerID: "OK-1", Name: "OK"}, {}}); err != nil {
		t.Fatal(err)
	}
	if len(v.List()) != 1 {
		t.Errorf("got %d entries, want 1 (blank row skipped)", len(v.List()))
	}
}
