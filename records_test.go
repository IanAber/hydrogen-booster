package main

import (
	"os"
	"testing"
)

func TestDispenseLogRecordAndDrain(t *testing.T) {
	path := t.TempDir() + "/dispense-log.jsonl"
	d := NewDispenseLog(path)

	if err := d.Record(DispenseRecord{CustomerCode: "5551234567", DeliveredMassKg: 1.234}); err != nil {
		t.Fatal(err)
	}
	if err := d.Record(DispenseRecord{CustomerCode: "5559876543", DeliveredMassKg: 2.5}); err != nil {
		t.Fatal(err)
	}

	records, err := d.DrainAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	if records[0].CustomerCode != "5551234567" || records[0].DeliveredMassKg != 1.234 {
		t.Errorf("records[0] = %+v", records[0])
	}
	if records[1].CustomerCode != "5559876543" || records[1].DeliveredMassKg != 2.5 {
		t.Errorf("records[1] = %+v", records[1])
	}

	// Draining must clear the file - a second drain gets nothing back.
	again, err := d.DrainAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("expected an empty drain after clearing, got %d records", len(again))
	}
}

func TestDispenseLogDrainMissingFileIsEmptyNotError(t *testing.T) {
	d := NewDispenseLog(t.TempDir() + "/does-not-exist.jsonl")
	records, err := d.DrainAll()
	if err != nil {
		t.Fatal(err)
	}
	if records != nil {
		t.Errorf("expected nil, got %v", records)
	}
}

func TestDispenseLogSkipsMalformedLine(t *testing.T) {
	path := t.TempDir() + "/dispense-log.jsonl"
	if err := os.WriteFile(path, []byte("not json\n{\"customerCode\":\"123\",\"deliveredMassKg\":1}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d := NewDispenseLog(path)
	records, err := d.DrainAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].CustomerCode != "123" {
		t.Errorf("got %+v, want one record for code 123", records)
	}
}

func TestValidCodesLookupAndIsValid(t *testing.T) {
	path := t.TempDir() + "/valid-codes.txt"
	if err := os.WriteFile(path, []byte("5551234567,Jane Doe\n5559876543,John Smith\n# a comment\n\n"), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := LoadValidCodes(path)
	if err != nil {
		t.Fatal(err)
	}

	name, ok := v.Lookup("5551234567")
	if !ok || name != "Jane Doe" {
		t.Errorf("Lookup(5551234567) = %q, %v; want Jane Doe, true", name, ok)
	}
	if !v.IsValid("5559876543") {
		t.Error("expected 5559876543 to be valid")
	}
	if v.IsValid("0000000000") {
		t.Error("expected an unlisted code to be invalid")
	}
}

func TestValidCodesMissingFileIsEmptyNotError(t *testing.T) {
	v, err := LoadValidCodes(t.TempDir() + "/does-not-exist.txt")
	if err != nil {
		t.Fatal(err)
	}
	if v.IsValid("5551234567") {
		t.Error("expected no codes to be valid against a missing file")
	}
	if len(v.List()) != 0 {
		t.Errorf("expected an empty list, got %v", v.List())
	}
}

func TestValidCodesSetAllPersistsAndReplaces(t *testing.T) {
	path := t.TempDir() + "/valid-codes.txt"
	v, err := LoadValidCodes(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := v.SetAll([]CodeEntry{
		{Code: "5551234567", Name: "Jane Doe"},
		{Code: "5559876543", Name: "John Smith"},
	}); err != nil {
		t.Fatal(err)
	}

	if !v.IsValid("5551234567") {
		t.Error("expected 5551234567 to be valid after SetAll")
	}
	list := v.List()
	if len(list) != 2 {
		t.Fatalf("got %d entries, want 2", len(list))
	}

	// A fresh load from disk must see the same data - confirms it was
	// actually persisted, not just kept in memory.
	v2, err := LoadValidCodes(path)
	if err != nil {
		t.Fatal(err)
	}
	if !v2.IsValid("5559876543") {
		t.Error("expected persisted codes to survive a fresh load")
	}

	// Replacing entirely drops anything not in the new set.
	if err := v.SetAll([]CodeEntry{{Code: "1112223333", Name: "New Customer"}}); err != nil {
		t.Fatal(err)
	}
	if v.IsValid("5551234567") {
		t.Error("expected the old code to be gone after SetAll replaced the list")
	}
	if !v.IsValid("1112223333") {
		t.Error("expected the new code to be valid")
	}
}

func TestValidCodesSetAllRejectsMalformedCode(t *testing.T) {
	path := t.TempDir() + "/valid-codes.txt"
	v, err := LoadValidCodes(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.SetAll([]CodeEntry{{Code: "1234567890", Name: "OK"}}); err != nil {
		t.Fatal(err)
	}

	if err := v.SetAll([]CodeEntry{{Code: "12a4", Name: "Bad"}}); err == nil {
		t.Error("expected an error for a non-digit code")
	}
	// The earlier good list must still be intact - a rejected SetAll
	// shouldn't partially apply or wipe the existing data.
	if !v.IsValid("1234567890") {
		t.Error("expected the previous valid list to survive a rejected SetAll")
	}
}

func TestValidCodesSetAllSkipsBlankRows(t *testing.T) {
	v, err := LoadValidCodes(t.TempDir() + "/valid-codes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.SetAll([]CodeEntry{
		{Code: "1234567890", Name: "OK"},
		{Code: "", Name: ""},
	}); err != nil {
		t.Fatal(err)
	}
	if len(v.List()) != 1 {
		t.Errorf("got %d entries, want 1 (blank row skipped)", len(v.List()))
	}
}
