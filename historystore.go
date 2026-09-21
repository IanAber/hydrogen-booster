package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// HistoryStore is the SQLite-backed history log for pressure/temperature
// readings and valve/relay/digital-input state changes. It's a pure
// storage layer - HistoryLogger decides what and when to log; the API
// handlers in webserver.go decide how to expose it.
//
// modernc.org/sqlite is used (rather than a cgo driver like
// mattn/go-sqlite3) because this project cross-builds dist/ARM64 from a
// Mac with no C cross-toolchain in the repo - a pure-Go driver keeps
// that build working unchanged.
type HistoryStore struct {
	db *sql.DB
}

// NewHistoryStore opens (creating if necessary) the SQLite database at
// path, sets pragmas suited to a single-writer/occasional-reader
// embedded workload, and ensures the schema exists. It fails fast on
// error - like LoadOrCreateAdminAuth/LoadValidCodes, a bad path is a
// startup misconfiguration worth surfacing immediately, not silently
// disabling history logging.
func NewHistoryStore(path string) (*HistoryStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open history db %s: %w", path, err)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("history db %s: %s: %w", path, pragma, err)
		}
	}
	const schema = `
CREATE TABLE IF NOT EXISTS sensor_readings (
	id      INTEGER PRIMARY KEY,
	ts      INTEGER NOT NULL,
	channel TEXT NOT NULL,
	value   REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sensor_readings_channel_ts ON sensor_readings(channel, ts);
CREATE INDEX IF NOT EXISTS idx_sensor_readings_ts ON sensor_readings(ts);

CREATE TABLE IF NOT EXISTS valve_states (
	id      INTEGER PRIMARY KEY,
	ts      INTEGER NOT NULL,
	channel TEXT NOT NULL,
	state   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_valve_states_channel_ts ON valve_states(channel, ts);
CREATE INDEX IF NOT EXISTS idx_valve_states_ts ON valve_states(ts);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("history db %s: create schema: %w", path, err)
	}
	return &HistoryStore{db: db}, nil
}

func (h *HistoryStore) Close() error {
	return h.db.Close()
}

func toUnixMs(t time.Time) int64 { return t.UnixMilli() }
func fromUnixMs(ms int64) time.Time {
	return time.UnixMilli(ms).UTC()
}

func (h *HistoryStore) InsertReading(ts time.Time, channel string, value float64) error {
	_, err := h.db.Exec(`INSERT INTO sensor_readings (ts, channel, value) VALUES (?, ?, ?)`,
		toUnixMs(ts), channel, value)
	return err
}

func (h *HistoryStore) InsertValveState(ts time.Time, channel string, state bool) error {
	_, err := h.db.Exec(`INSERT INTO valve_states (ts, channel, state) VALUES (?, ?, ?)`,
		toUnixMs(ts), channel, boolToInt(state))
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ReadingPoint is one sampled value at a point in time.
type ReadingPoint struct {
	Ts    time.Time `json:"ts"`
	Value float64   `json:"value"`
}

// StatePoint is one valve/relay/digital-input state, at the moment it
// took effect (either an actual transition, or - for the first point
// returned by QueryValveStates - the state already in effect at the
// start of the requested window).
type StatePoint struct {
	Ts    time.Time `json:"ts"`
	State bool      `json:"state"`
}

// QueryReadings returns every sensor_readings row for channel with
// from <= ts <= to, oldest first, capped at limit rows.
func (h *HistoryStore) QueryReadings(channel string, from, to time.Time, limit int) ([]ReadingPoint, error) {
	rows, err := h.db.Query(
		`SELECT ts, value FROM sensor_readings WHERE channel = ? AND ts >= ? AND ts <= ? ORDER BY ts ASC LIMIT ?`,
		channel, toUnixMs(from), toUnixMs(to), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var points []ReadingPoint
	for rows.Next() {
		var ms int64
		var v float64
		if err := rows.Scan(&ms, &v); err != nil {
			return nil, err
		}
		points = append(points, ReadingPoint{Ts: fromUnixMs(ms), Value: v})
	}
	return points, rows.Err()
}

// QueryValveStates returns valve_states rows for channel across
// [from, to], oldest first, capped at limit. Since valve_states only
// records a row on an actual transition, the state in effect at the
// very start of the window may have been set before `from` - so this
// prepends that carried-forward state (if any row before `from`
// exists) as the first point, letting a caller reconstruct the state
// throughout the whole window without a blank gap before the first
// in-window transition.
func (h *HistoryStore) QueryValveStates(channel string, from, to time.Time, limit int) ([]StatePoint, error) {
	var prior *StatePoint
	row := h.db.QueryRow(
		`SELECT ts, state FROM valve_states WHERE channel = ? AND ts < ? ORDER BY ts DESC LIMIT 1`,
		channel, toUnixMs(from),
	)
	var ms int64
	var st int
	switch err := row.Scan(&ms, &st); err {
	case nil:
		prior = &StatePoint{Ts: fromUnixMs(ms), State: st != 0}
	case sql.ErrNoRows:
		// No state recorded before the window - nothing to carry forward.
	default:
		return nil, err
	}

	rows, err := h.db.Query(
		`SELECT ts, state FROM valve_states WHERE channel = ? AND ts >= ? AND ts <= ? ORDER BY ts ASC LIMIT ?`,
		channel, toUnixMs(from), toUnixMs(to), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]StatePoint, 0, limit)
	if prior != nil {
		result = append(result, *prior)
	}
	for rows.Next() {
		var ms int64
		var st int
		if err := rows.Scan(&ms, &st); err != nil {
			return nil, err
		}
		result = append(result, StatePoint{Ts: fromUnixMs(ms), State: st != 0})
	}
	return result, rows.Err()
}

// Prune deletes every row older than cutoff from both tables -
// enforces the retention window (see HistoryLogger).
func (h *HistoryStore) Prune(cutoff time.Time) error {
	cutoffMs := toUnixMs(cutoff)
	if _, err := h.db.Exec(`DELETE FROM sensor_readings WHERE ts < ?`, cutoffMs); err != nil {
		return fmt.Errorf("prune sensor_readings: %w", err)
	}
	if _, err := h.db.Exec(`DELETE FROM valve_states WHERE ts < ?`, cutoffMs); err != nil {
		return fmt.Errorf("prune valve_states: %w", err)
	}
	return nil
}
