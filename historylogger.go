package main

import (
	"fmt"
	"time"
)

// HistoryLogger samples pressure/temperature readings on a fixed
// interval and logs valve/relay/digital-input states only on change,
// writing both to a HistoryStore. It runs continuously from process
// start, independent of job phase - a background trend/audit log, not
// a per-fill record (that's DispenseLog).
type HistoryLogger struct {
	store          *HistoryStore
	io             IO
	sampleInterval time.Duration
	retention      time.Duration

	lastState map[string]bool
	lastPrune time.Time
	errLog    errorLatch // logs database write failures once each, and their recovery
}

// pruneCheckInterval is how often HistoryLogger checks whether it's
// time to prune old rows - deliberately coarse (pruning is a DELETE
// over a range of a large table) compared to the sampling interval.
const pruneCheckInterval = time.Hour

func NewHistoryLogger(store *HistoryStore, io IO, sampleInterval, retention time.Duration) *HistoryLogger {
	return &HistoryLogger{
		store: store, io: io,
		sampleInterval: sampleInterval, retention: retention,
		lastState: map[string]bool{},
	}
}

// Run should be called once in a goroutine for the life of the
// program.
func (h *HistoryLogger) Run() {
	t := time.NewTicker(h.sampleInterval)
	defer t.Stop()
	for now := range t.C {
		h.tick(now)
	}
}

// tick is the testable unit of work - see historylogger_test.go, which
// drives it directly rather than waiting on a real ticker.
func (h *HistoryLogger) tick(now time.Time) {
	for _, s := range allPressureSensors {
		if v, err := h.io.ReadPressureBar(s); err == nil {
			if err := h.store.InsertReading(now, s.String(), v); err != nil {
				h.errLog.report("history database", fmt.Errorf("insert reading %s: %w", s, err))
			}
		}
	}
	for _, s := range allTempSensors {
		if v, err := h.io.ReadTempC(s); err == nil {
			if err := h.store.InsertReading(now, s.String(), v); err != nil {
				h.errLog.report("history database", fmt.Errorf("insert reading %s: %w", s, err))
			}
		}
	}

	for _, r := range allRelays {
		if on, err := h.io.RelayState(r); err == nil {
			h.logIfChanged(now, r.String(), on)
		}
	}
	for _, d := range allDigitalInputs {
		if v, err := h.io.ReadDigitalInput(d); err == nil {
			h.logIfChanged(now, d.String(), v)
		}
	}

	if h.lastPrune.IsZero() {
		h.lastPrune = now
	} else if now.Sub(h.lastPrune) >= pruneCheckInterval {
		if err := h.store.Prune(now.Add(-h.retention)); err != nil {
			logErrorf("history database: prune: %v", err)
		}
		h.lastPrune = now
	}
}

// logIfChanged records a valve_states row only the first time a
// channel is observed, or when its state differs from the last
// recorded value - see HistoryLogger's doc comment on why relay/
// digital-input channels are event-based rather than sampled.
func (h *HistoryLogger) logIfChanged(now time.Time, channel string, state bool) {
	if prev, ok := h.lastState[channel]; ok && prev == state {
		return
	}
	h.lastState[channel] = state
	if err := h.store.InsertValveState(now, channel, state); err != nil {
		h.errLog.report("history database", fmt.Errorf("insert valve state %s: %w", channel, err))
	}
}
