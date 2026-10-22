package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// DispenseRecord is one completed fill: when, which customer, and how
// much hydrogen went out - for billing/reconciliation. Deliberately has
// no login code: the customer is identified by ID and name, captured at
// the time of the fill, so a later rename in the codes file doesn't
// rewrite history.
type DispenseRecord struct {
	ID              int64     `json:"id"`
	Timestamp       time.Time `json:"timestamp"`
	CustomerID      string    `json:"customerId"`
	CustomerName    string    `json:"customerName"`
	DeliveredMassKg float64   `json:"deliveredMassKg"`
}

// DispenseStore is the SQLite database of completed fills. Records are
// kept permanently - never pruned, unlike HistoryStore's readings - and
// read back by date range (see /api/records), so extracting twice gives
// the same answer and a failed download loses nothing.
type DispenseStore struct {
	db *sql.DB
}

// NewDispenseStore opens (creating if necessary) the dispense database
// at path. Like NewHistoryStore it fails fast on error - billing data
// silently not being recorded is worse than a visible startup failure.
func NewDispenseStore(path string) (*DispenseStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open dispense db %s: %w", path, err)
	}
	for _, stmt := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL", // billing data - durability over speed; a fill is a rare write
		"PRAGMA busy_timeout=5000",
		`CREATE TABLE IF NOT EXISTS dispenses (
			id            INTEGER PRIMARY KEY,
			ts            INTEGER NOT NULL,
			customer_id   TEXT NOT NULL,
			customer_name TEXT NOT NULL,
			delivered_kg  REAL NOT NULL
		)`,
		"CREATE INDEX IF NOT EXISTS idx_dispenses_ts ON dispenses(ts)",
		"CREATE INDEX IF NOT EXISTS idx_dispenses_customer_ts ON dispenses(customer_id, ts)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("dispense db %s: %w", path, err)
		}
	}
	return &DispenseStore{db: db}, nil
}

func (d *DispenseStore) Close() error { return d.db.Close() }

// Record stores one completed fill.
func (d *DispenseStore) Record(rec DispenseRecord) error {
	_, err := d.db.Exec(`INSERT INTO dispenses (ts, customer_id, customer_name, delivered_kg) VALUES (?, ?, ?, ?)`,
		toUnixMs(rec.Timestamp), rec.CustomerID, rec.CustomerName, rec.DeliveredMassKg)
	if err != nil {
		return fmt.Errorf("recording dispense: %w", err)
	}
	return nil
}

// Query returns fills with from <= timestamp < to, oldest first; a zero
// from or to leaves that end open, and a non-empty customerID limits it
// to that customer.
func (d *DispenseStore) Query(from, to time.Time, customerID string) ([]DispenseRecord, error) {
	q := `SELECT id, ts, customer_id, customer_name, delivered_kg FROM dispenses WHERE 1=1`
	var args []any
	if !from.IsZero() {
		q += ` AND ts >= ?`
		args = append(args, toUnixMs(from))
	}
	if !to.IsZero() {
		q += ` AND ts < ?`
		args = append(args, toUnixMs(to))
	}
	if customerID != "" {
		q += ` AND customer_id = ?`
		args = append(args, customerID)
	}
	q += ` ORDER BY ts, id`
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("querying dispenses: %w", err)
	}
	defer rows.Close()
	records := []DispenseRecord{}
	for rows.Next() {
		var r DispenseRecord
		var ts int64
		if err := rows.Scan(&r.ID, &ts, &r.CustomerID, &r.CustomerName, &r.DeliveredMassKg); err != nil {
			return nil, fmt.Errorf("querying dispenses: %w", err)
		}
		r.Timestamp = fromUnixMs(ts)
		records = append(records, r)
	}
	return records, rows.Err()
}

// legacyDispenseRecord is a line of the old dispense-log.jsonl file,
// which stored the raw login code.
type legacyDispenseRecord struct {
	Timestamp       time.Time `json:"timestamp"`
	CustomerCode    string    `json:"customerCode"`
	DeliveredMassKg float64   `json:"deliveredMassKg"`
}

// ImportLegacyDispenseLog moves any records left in the old
// dispense-log.jsonl file (from before this database existed) into the
// database, translating each login code to its customer via codes, and
// then deletes the file - it holds login codes, which must not be
// stored anywhere but the codes file. A code that's no longer on the
// list is recorded as customer "UNKNOWN". Returns how many records were
// imported; a missing or empty file is not an error. The import is one
// transaction: if anything fails, nothing is imported and the file is
// left in place for the next startup to retry, without duplicates.
func (d *DispenseStore) ImportLegacyDispenseLog(path string, codes *ValidCodes) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading %s: %w", path, err)
	}

	tx, err := d.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("importing %s: %w", path, err)
	}
	defer tx.Rollback()
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var old legacyDispenseRecord
		if err := json.Unmarshal([]byte(line), &old); err != nil {
			continue // a torn write from a crash - nothing recoverable
		}
		rec := DispenseRecord{Timestamp: old.Timestamp, DeliveredMassKg: old.DeliveredMassKg}
		if old.CustomerCode != "" {
			rec.CustomerID = "UNKNOWN"
			if codes != nil {
				if c, ok := codes.Lookup(old.CustomerCode); ok {
					rec.CustomerID, rec.CustomerName = c.ID, c.Name
				}
			}
		}
		if _, err := tx.Exec(`INSERT INTO dispenses (ts, customer_id, customer_name, delivered_kg) VALUES (?, ?, ?, ?)`,
			toUnixMs(rec.Timestamp), rec.CustomerID, rec.CustomerName, rec.DeliveredMassKg); err != nil {
			return 0, fmt.Errorf("importing %s: %w", path, err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("importing %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return n, fmt.Errorf("imported %d record(s) but could not delete %s: %w", n, path, err)
	}
	return n, nil
}
