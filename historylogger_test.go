package main

import (
	"testing"
	"time"
)

// fakeHistoryIO is a minimal in-memory IO fake for testing HistoryLogger,
// with every reading directly settable (unlike fakeStageIO in
// stage_test.go, whose ReadTempC/ReadDigitalInput are fixed stubs).
type fakeHistoryIO struct {
	relays   map[RelayOutput]bool
	pressure map[SensorInput]float64
	temp     map[SensorInput]float64
	digital  map[DigitalInput]bool
}

func newFakeHistoryIO() *fakeHistoryIO {
	return &fakeHistoryIO{
		relays:   map[RelayOutput]bool{},
		pressure: map[SensorInput]float64{},
		temp:     map[SensorInput]float64{},
		digital:  map[DigitalInput]bool{},
	}
}

func (f *fakeHistoryIO) SetRelay(r RelayOutput, on bool) error  { f.relays[r] = on; return nil }
func (f *fakeHistoryIO) RelayState(r RelayOutput) (bool, error) { return f.relays[r], nil }
func (f *fakeHistoryIO) ReadPressureBar(s SensorInput) (float64, error) {
	return f.pressure[s], nil
}
func (f *fakeHistoryIO) ReadTempC(s SensorInput) (float64, error)      { return f.temp[s], nil }
func (f *fakeHistoryIO) ReadDigitalInput(d DigitalInput) (bool, error) { return f.digital[d], nil }
func (f *fakeHistoryIO) Close() error                                  { return nil }

func TestHistoryLoggerLogsEveryReadingChannelEachTick(t *testing.T) {
	store := newTestHistoryStore(t)
	io := newFakeHistoryIO()
	io.pressure[Hyd1Pressure] = 100
	io.temp[OutputTemp] = 30

	logger := NewHistoryLogger(store, io, time.Second, 30*24*time.Hour)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	logger.tick(base)
	logger.tick(base.Add(time.Second)) // same readings again - should still log both times

	points, err := store.QueryReadings("Hyd1Pressure", base.Add(-time.Minute), base.Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("QueryReadings: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("Hyd1Pressure got %d rows, want 2 (readings are sampled unconditionally every tick)", len(points))
	}
	for _, p := range points {
		if p.Value != 100 {
			t.Fatalf("Hyd1Pressure point = %+v, want value 100", p)
		}
	}

	tempPoints, err := store.QueryReadings("OutputTemp", base.Add(-time.Minute), base.Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("QueryReadings: %v", err)
	}
	if len(tempPoints) != 2 {
		t.Fatalf("OutputTemp got %d rows, want 2", len(tempPoints))
	}
}

func TestHistoryLoggerLogsValveStatesOnlyOnChange(t *testing.T) {
	store := newTestHistoryStore(t)
	io := newFakeHistoryIO()
	io.relays[Pump1] = false
	io.digital[Motor1Powered] = true

	logger := NewHistoryLogger(store, io, time.Second, 30*24*time.Hour)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	logger.tick(base) // first observation of every channel - always logged once

	// Same values, repeated ticks - no new rows.
	logger.tick(base.Add(time.Second))
	logger.tick(base.Add(2 * time.Second))

	points, err := store.QueryValveStates("Pump1", base.Add(-time.Minute), base.Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("QueryValveStates: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("Pump1 got %d rows after unchanged ticks, want 1 (only the first observation)", len(points))
	}
	if points[0].State {
		t.Fatalf("Pump1 state = true, want false")
	}

	// Now actually change it - should log exactly one more row.
	io.relays[Pump1] = true
	logger.tick(base.Add(3 * time.Second))

	points, err = store.QueryValveStates("Pump1", base.Add(-time.Minute), base.Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("QueryValveStates: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("Pump1 got %d rows after a real change, want 2", len(points))
	}
	if !points[1].State {
		t.Fatalf("Pump1's second point state = false, want true")
	}

	// Motor1Powered never changed - still just its one initial observation.
	motorPoints, err := store.QueryValveStates("Motor1Powered", base.Add(-time.Minute), base.Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("QueryValveStates: %v", err)
	}
	if len(motorPoints) != 1 {
		t.Fatalf("Motor1Powered got %d rows, want 1 (never changed)", len(motorPoints))
	}
}

func TestHistoryLoggerPrunesOnceCheckIntervalElapses(t *testing.T) {
	store := newTestHistoryStore(t)
	io := newFakeHistoryIO()

	retention := 24 * time.Hour
	logger := NewHistoryLogger(store, io, time.Second, retention)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldTs := base.Add(-48 * time.Hour) // well past the retention window once pruned
	recentTs := base                   // within the retention window throughout

	if err := store.InsertReading(oldTs, "TestChannel", 1); err != nil {
		t.Fatalf("InsertReading: %v", err)
	}
	if err := store.InsertReading(recentTs, "TestChannel", 2); err != nil {
		t.Fatalf("InsertReading: %v", err)
	}

	logger.tick(base) // first tick only establishes lastPrune - no prune yet

	points, err := store.QueryReadings("TestChannel", oldTs.Add(-time.Hour), base.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("QueryReadings: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("got %d rows before the check interval elapses, want 2 (no prune yet)", len(points))
	}

	// Advance past pruneCheckInterval - this tick's cutoff is
	// (base+pruneCheckInterval)-retention, which is after oldTs but
	// before recentTs.
	logger.tick(base.Add(pruneCheckInterval))

	points, err = store.QueryReadings("TestChannel", oldTs.Add(-time.Hour), base.Add(2*time.Hour), 100)
	if err != nil {
		t.Fatalf("QueryReadings: %v", err)
	}
	if len(points) != 1 || points[0].Value != 2 {
		t.Fatalf("got %+v after the prune check interval elapsed, want only the recent (value=2) row", points)
	}
}
