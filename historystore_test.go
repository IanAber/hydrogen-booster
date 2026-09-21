package main

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestHistoryStore(t *testing.T) *HistoryStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := NewHistoryStore(path)
	if err != nil {
		t.Fatalf("NewHistoryStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestHistoryStoreReadingsRoundTrip(t *testing.T) {
	store := newTestHistoryStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if err := store.InsertReading(base, "Hyd1Pressure", 20.0); err != nil {
		t.Fatalf("InsertReading: %v", err)
	}
	if err := store.InsertReading(base.Add(10*time.Second), "Hyd1Pressure", 25.0); err != nil {
		t.Fatalf("InsertReading: %v", err)
	}
	// A different channel shouldn't show up in Hyd1Pressure's query.
	if err := store.InsertReading(base.Add(5*time.Second), "OutputTemp", 30.0); err != nil {
		t.Fatalf("InsertReading: %v", err)
	}

	points, err := store.QueryReadings("Hyd1Pressure", base.Add(-time.Minute), base.Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("QueryReadings: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("got %d points, want 2", len(points))
	}
	if points[0].Value != 20.0 || points[1].Value != 25.0 {
		t.Fatalf("points = %+v, want [20.0, 25.0] in order", points)
	}
	if !points[0].Ts.Equal(base) {
		t.Fatalf("points[0].Ts = %v, want %v", points[0].Ts, base)
	}
}

func TestHistoryStoreReadingsRespectsFromToAndLimit(t *testing.T) {
	store := newTestHistoryStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		if err := store.InsertReading(ts, "OutputTemp", float64(i)); err != nil {
			t.Fatalf("InsertReading: %v", err)
		}
	}

	// Window covering only the middle three samples (minutes 1-3).
	points, err := store.QueryReadings("OutputTemp", base.Add(time.Minute), base.Add(3*time.Minute), 100)
	if err != nil {
		t.Fatalf("QueryReadings: %v", err)
	}
	if len(points) != 3 {
		t.Fatalf("got %d points, want 3", len(points))
	}
	if points[0].Value != 1 || points[2].Value != 3 {
		t.Fatalf("points = %+v, want values 1,2,3", points)
	}

	// Same window, but limited to 2 rows.
	limited, err := store.QueryReadings("OutputTemp", base.Add(time.Minute), base.Add(3*time.Minute), 2)
	if err != nil {
		t.Fatalf("QueryReadings: %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("got %d points, want 2 (limit)", len(limited))
	}
}

func TestHistoryStoreValveStatesRoundTripAndCarriesForwardPriorState(t *testing.T) {
	store := newTestHistoryStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// A transition well before the query window - should be carried
	// forward as the first point.
	if err := store.InsertValveState(base, "Pump1", true); err != nil {
		t.Fatalf("InsertValveState: %v", err)
	}
	// A transition inside the query window.
	if err := store.InsertValveState(base.Add(time.Hour), "Pump1", false); err != nil {
		t.Fatalf("InsertValveState: %v", err)
	}

	points, err := store.QueryValveStates("Pump1", base.Add(30*time.Minute), base.Add(2*time.Hour), 100)
	if err != nil {
		t.Fatalf("QueryValveStates: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("got %d points, want 2 (carried-forward prior state + 1 in-window transition): %+v", len(points), points)
	}
	if !points[0].State {
		t.Fatalf("points[0].State = false, want true (carried forward from before the window)")
	}
	if !points[0].Ts.Equal(base) {
		t.Fatalf("points[0].Ts = %v, want %v (the prior transition's own timestamp)", points[0].Ts, base)
	}
	if points[1].State {
		t.Fatalf("points[1].State = true, want false (the in-window transition)")
	}
}

func TestHistoryStoreValveStatesNoPriorState(t *testing.T) {
	store := newTestHistoryStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Only an in-window transition - nothing to carry forward.
	if err := store.InsertValveState(base.Add(time.Minute), "Valve1A(dir)", true); err != nil {
		t.Fatalf("InsertValveState: %v", err)
	}

	points, err := store.QueryValveStates("Valve1A(dir)", base, base.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("QueryValveStates: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("got %d points, want 1 (no prior state to carry forward): %+v", len(points), points)
	}
}

func TestHistoryStorePruneRemovesOnlyOlderRows(t *testing.T) {
	store := newTestHistoryStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cutoff := base.Add(time.Hour)

	if err := store.InsertReading(base, "Hyd1Pressure", 1); err != nil {
		t.Fatalf("InsertReading: %v", err)
	}
	if err := store.InsertReading(base.Add(2*time.Hour), "Hyd1Pressure", 2); err != nil {
		t.Fatalf("InsertReading: %v", err)
	}
	if err := store.InsertValveState(base, "Pump1", true); err != nil {
		t.Fatalf("InsertValveState: %v", err)
	}
	if err := store.InsertValveState(base.Add(2*time.Hour), "Pump1", false); err != nil {
		t.Fatalf("InsertValveState: %v", err)
	}

	if err := store.Prune(cutoff); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	readings, err := store.QueryReadings("Hyd1Pressure", base.Add(-time.Hour), base.Add(3*time.Hour), 100)
	if err != nil {
		t.Fatalf("QueryReadings: %v", err)
	}
	if len(readings) != 1 || readings[0].Value != 2 {
		t.Fatalf("readings after prune = %+v, want only the post-cutoff row", readings)
	}

	states, err := store.QueryValveStates("Pump1", base.Add(-time.Hour), base.Add(3*time.Hour), 100)
	if err != nil {
		t.Fatalf("QueryValveStates: %v", err)
	}
	if len(states) != 1 || states[0].State {
		t.Fatalf("states after prune = %+v, want only the post-cutoff (false) row", states)
	}
}
