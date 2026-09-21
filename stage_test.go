package main

import (
	"sync"
	"testing"
	"time"
)

// fakeStageIO is a minimal in-memory IO for testing Stages without real
// hardware or SimulatedIO's background-goroutine physics - pressure per
// sensor and relay state are both directly controllable/inspectable.
type fakeStageIO struct {
	mu       sync.Mutex
	relays   map[RelayOutput]bool
	pressure map[SensorInput]float64
}

func newFakeStageIO() *fakeStageIO {
	return &fakeStageIO{relays: map[RelayOutput]bool{}, pressure: map[SensorInput]float64{}}
}

func (f *fakeStageIO) SetRelay(r RelayOutput, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relays[r] = on
	return nil
}

func (f *fakeStageIO) RelayState(r RelayOutput) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.relays[r], nil
}

func (f *fakeStageIO) ReadPressureBar(s SensorInput) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pressure[s], nil
}

func (f *fakeStageIO) ReadTempC(s SensorInput) (float64, error)      { return 0, nil }
func (f *fakeStageIO) ReadDigitalInput(d DigitalInput) (bool, error) { return false, nil }
func (f *fakeStageIO) Close() error                                  { return nil }

func (f *fakeStageIO) setPressure(s SensorInput, v float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pressure[s] = v
}

func (f *fakeStageIO) relayOn(r RelayOutput) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.relays[r]
}

// shortHomingConfig gives both stages a short MaxStrokeTime (so tests
// don't need to simulate a realistic pressure-spike sequence) and a
// short Homing.Duration, so tests can advance through homing and
// several strokes using small synthetic time increments.
func shortHomingConfig() Config {
	cfg := DefaultConfig()
	cfg.Homing.Duration = 100 * time.Millisecond
	cfg.Stage1.MaxStrokeTime = 50 * time.Millisecond
	cfg.Stage1.MinStrokeTime = 0
	cfg.Stage2.MaxStrokeTime = 50 * time.Millisecond
	cfg.Stage2.MinStrokeTime = 0
	cfg.Stage1.ReverseDelay = 20 * time.Millisecond
	cfg.Stage2.ReverseDelay = 20 * time.Millisecond
	return cfg
}

func TestStagesHomingForcesDirectionOffReliefOnUntilDurationElapses(t *testing.T) {
	live := NewLiveConfig(shortHomingConfig())
	io := newFakeStageIO()
	s := NewStages(live, io)

	start := time.Now()
	s.Start()

	if io.relayOn(Valve1A) || io.relayOn(Valve2A) {
		t.Fatal("direction valves should be forced off at the start of homing")
	}
	if !io.relayOn(Valve1B) || !io.relayOn(Valve2B) {
		t.Fatal("relief valves should be open at the start of homing")
	}

	// Still within the homing window - nothing should change.
	s.Tick(start.Add(50 * time.Millisecond))
	if !io.relayOn(Valve1B) || !io.relayOn(Valve2B) {
		t.Fatal("relief valves should still be open before homing duration elapses")
	}
	if io.relayOn(Valve1A) || io.relayOn(Valve2A) {
		t.Fatal("direction valves should still be off before homing duration elapses")
	}

	// Past the homing window - relief should close to begin the first stroke.
	s.Tick(start.Add(150 * time.Millisecond))
	if io.relayOn(Valve1B) || io.relayOn(Valve2B) {
		t.Fatal("relief valves should close once homing duration has elapsed")
	}
	if io.relayOn(Valve1A) || io.relayOn(Valve2A) {
		t.Fatal("direction valves should not flip just because homing ended")
	}
}

func TestStagesBarrierWaitsForBothCircuitsBeforeReversing(t *testing.T) {
	cfg := shortHomingConfig()
	cfg.Homing.Duration = 0 // skip homing immediately
	// Circuit 1 times out quickly; circuit 2 takes much longer.
	cfg.Stage1.MaxStrokeTime = 10 * time.Millisecond
	cfg.Stage2.MaxStrokeTime = 200 * time.Millisecond
	live := NewLiveConfig(cfg)
	io := newFakeStageIO()
	s := NewStages(live, io)

	s.Start()
	start := time.Now()
	s.Tick(start) // homing duration is 0, so this ends homing and closes both relief valves

	if io.relayOn(Valve1B) || io.relayOn(Valve2B) {
		t.Fatal("relief valves should be closed once homing ends immediately")
	}

	// Circuit 1's MaxStrokeTime has elapsed, circuit 2's has not.
	s.Tick(start.Add(20 * time.Millisecond))
	if !io.relayOn(Valve1B) {
		t.Fatal("circuit 1 should have re-opened its relief valve at the barrier")
	}
	if io.relayOn(Valve2B) {
		t.Fatal("circuit 2 should still be driving (relief closed)")
	}
	if io.relayOn(Valve1A) {
		t.Fatal("circuit 1's direction valve must not flip while circuit 2 is still driving")
	}

	// Now circuit 2's MaxStrokeTime has also elapsed - both should reverse together.
	s.Tick(start.Add(210 * time.Millisecond))
	if !io.relayOn(Valve2B) {
		t.Fatal("circuit 2 should have re-opened its relief valve at the barrier")
	}
	if !io.relayOn(Valve1A) || !io.relayOn(Valve2A) {
		t.Fatal("both direction valves should flip together once both circuits reach the barrier")
	}
	s1, s2 := s.Status()
	if s1.StrokeCount != 1 || s2.StrokeCount != 1 {
		t.Fatalf("stroke counts = %d, %d; want 1, 1 (both reverse together)", s1.StrokeCount, s2.StrokeCount)
	}
	if s1.Direction != "UP" || s2.Direction != "UP" {
		t.Fatalf("directions = %s, %s; want UP, UP", s1.Direction, s2.Direction)
	}
}

func TestStagesReverseDelayBeforeReliefRecloses(t *testing.T) {
	cfg := shortHomingConfig()
	cfg.Homing.Duration = 0
	cfg.Stage1.MaxStrokeTime = 10 * time.Millisecond
	cfg.Stage2.MaxStrokeTime = 10 * time.Millisecond
	cfg.Stage1.ReverseDelay = 30 * time.Millisecond
	cfg.Stage2.ReverseDelay = 30 * time.Millisecond
	live := NewLiveConfig(cfg)
	io := newFakeStageIO()
	s := NewStages(live, io)

	s.Start()
	start := time.Now()
	s.Tick(start) // end homing

	// Both circuits reach the barrier and reverse together.
	s.Tick(start.Add(20 * time.Millisecond))
	if !io.relayOn(Valve1A) || !io.relayOn(Valve2A) {
		t.Fatal("expected both direction valves to have flipped by now")
	}
	if !io.relayOn(Valve1B) || !io.relayOn(Valve2B) {
		t.Fatal("relief valves should still be open immediately after reversing")
	}

	// Before ReverseDelay elapses, relief should stay open.
	s.Tick(start.Add(30 * time.Millisecond))
	if !io.relayOn(Valve1B) || !io.relayOn(Valve2B) {
		t.Fatal("relief valves should remain open until ReverseDelay elapses")
	}

	// After ReverseDelay elapses, relief closes and the next stroke begins.
	s.Tick(start.Add(60 * time.Millisecond))
	if io.relayOn(Valve1B) || io.relayOn(Valve2B) {
		t.Fatal("relief valves should close once ReverseDelay has elapsed")
	}
}

func TestStagesPressureSpikeEndsStroke(t *testing.T) {
	cfg := shortHomingConfig()
	cfg.Homing.Duration = 0
	cfg.Stage1.MaxStrokeTime = time.Hour // effectively disabled, so only the spike can end the stroke
	cfg.Stage1.SpikeDeltaBar = 10
	cfg.Stage1.SpikeWindow = 100 * time.Millisecond
	cfg.Stage1.MinStrokeTime = 0
	cfg.Stage2.MaxStrokeTime = time.Hour
	cfg.Stage2.SpikeDeltaBar = 10
	cfg.Stage2.SpikeWindow = 100 * time.Millisecond
	cfg.Stage2.MinStrokeTime = 0
	live := NewLiveConfig(cfg)
	io := newFakeStageIO()
	s := NewStages(live, io)

	s.Start()
	start := time.Now()
	s.Tick(start) // end homing
	io.setPressure(Hyd1Pressure, 20)
	io.setPressure(Hyd2Pressure, 20)
	s.Tick(start.Add(10 * time.Millisecond))

	// A rise of >= SpikeDeltaBar within SpikeWindow should end the stroke.
	io.setPressure(Hyd1Pressure, 35)
	io.setPressure(Hyd2Pressure, 35)
	s.Tick(start.Add(20 * time.Millisecond))

	if !io.relayOn(Valve1A) || !io.relayOn(Valve2A) {
		t.Fatal("a pressure spike on both circuits should have ended the stroke and reversed both")
	}
}

func TestStagesMedianFilterRejectsIsolatedNoiseSpike(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Homing.Duration = 0
	cfg.Stage1.PressureFilterSamples = 3
	cfg.Stage1.MaxStrokeTime = time.Hour // only the spike detector should be able to end this stroke
	cfg.Stage1.MinStrokeTime = 0
	cfg.Stage1.SpikeDeltaBar = 10
	cfg.Stage1.SpikeWindow = time.Hour // keep all samples in the comparison window for the test's duration
	cfg.Stage2.MaxStrokeTime = time.Hour
	live := NewLiveConfig(cfg)
	io := newFakeStageIO()
	s := NewStages(live, io)

	s.Start()
	start := time.Now()
	// Seed the sensor with the baseline reading *before* the tick that ends
	// homing and begins driving, so that first driving-phase sample - taken
	// within this same Tick call - is the intended baseline, not a phantom
	// zero reading from the not-yet-configured fake sensor.
	io.setPressure(Hyd1Pressure, 20)
	s.Tick(start) // Homing.Duration is 0, so this ends homing immediately; history[0]=(0ms,20)

	step := func(p float64, elapsedMs int) {
		io.setPressure(Hyd1Pressure, p)
		s.Tick(start.Add(time.Duration(elapsedMs) * time.Millisecond))
	}

	step(20, 10) // filterBuf=[20,20]

	step(35, 20) // filterBuf=[20,20,35], median=20 - a single noisy sample
	if io.relayOn(Valve1B) {
		t.Fatal("an isolated single-sample noise spike should be rejected by the median filter")
	}
	step(20, 30) // filterBuf=[20,35,20], median=20 - back to baseline
	if io.relayOn(Valve1B) {
		t.Fatal("stroke should still be driving once the isolated spike has scrolled out of the filter")
	}
	step(20, 40) // filterBuf=[35,20,20], median=20
	step(20, 50) // filterBuf=[20,20,20], median=20 - fully flushed

	step(35, 60) // filterBuf=[20,20,35], median=20 - one genuine high sample, still a minority
	if io.relayOn(Valve1B) {
		t.Fatal("a single high sample should not yet flip the median")
	}
	step(35, 70) // filterBuf=[20,35,35], median=35 - a sustained rise, now the majority
	if !io.relayOn(Valve1B) {
		t.Fatal("a sustained rise (majority of the filter window) should still be detected as a spike")
	}
}

func TestStagesPressureBlankTimeSuppressesSpikeAfterSwitch(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Homing.Duration = 0
	cfg.PressureBlankTime = 500 * time.Millisecond
	cfg.Stage1.MaxStrokeTime = time.Hour
	cfg.Stage1.MinStrokeTime = 0
	cfg.Stage1.SpikeDeltaBar = 10
	cfg.Stage1.SpikeWindow = time.Hour
	cfg.Stage2.MaxStrokeTime = time.Hour
	live := NewLiveConfig(cfg)
	io := newFakeStageIO()
	s := NewStages(live, io)

	s.Start()
	start := time.Now()
	io.setPressure(Hyd1Pressure, 20) // baseline, seeded before the homing-ending tick (see median filter test)
	s.Tick(start)                    // ends homing immediately, and records this relay switch as lastSwitch

	io.setPressure(Hyd1Pressure, 35) // a spike-sized jump, but right after the homing->driving switch
	s.Tick(start.Add(10 * time.Millisecond))
	if io.relayOn(Valve1B) {
		t.Fatal("a spike-sized reading arriving within the post-switch blank window should be suppressed")
	}

	// Same elevated reading, but now past the blank window.
	s.Tick(start.Add(520 * time.Millisecond))
	if !io.relayOn(Valve1B) {
		t.Fatal("the same reading should count as a spike once the post-switch blank window has elapsed")
	}
}

func TestStagesStopDeenergisesAllFourRelays(t *testing.T) {
	live := NewLiveConfig(shortHomingConfig())
	io := newFakeStageIO()
	s := NewStages(live, io)

	s.Start()
	s.Stop()

	for _, r := range []RelayOutput{Valve1A, Valve1B, Valve2A, Valve2B} {
		if io.relayOn(r) {
			t.Errorf("%s should be de-energised after Stop", r)
		}
	}
	if s.IsRunning() {
		t.Error("IsRunning should be false after Stop")
	}
}
