package main

import "testing"

// TestSimulatedIOFrozenWhileNotRunning guards against the physics model
// drifting on its own between jobs (or before any job has ever run) -
// exactly like real hardware with the pump off, hydraulic ram pressure
// and tank depletion shouldn't move. OutputPressure is exempt - see
// TestSimulatedIOOutputPressureReleasesWhenNotRunning: it deliberately
// bleeds back toward 0 while not running, mimicking the delivery hose
// releasing after a fill.
func TestSimulatedIOFrozenWhileNotRunning(t *testing.T) {
	s := NewSimulatedIO()
	defer s.Close()

	hyd1Before, _ := s.ReadPressureBar(Hyd1Pressure)
	tankBefore, _ := s.ReadPressureBar(TankPressure)

	for i := 0; i < 50; i++ {
		s.step()
	}

	hyd1After, _ := s.ReadPressureBar(Hyd1Pressure)
	tankAfter, _ := s.ReadPressureBar(TankPressure)

	if hyd1After != hyd1Before {
		t.Errorf("Hyd1Pressure drifted while not running: %v -> %v", hyd1Before, hyd1After)
	}
	if tankAfter != tankBefore {
		t.Errorf("TankPressure drifted while not running: %v -> %v", tankBefore, tankAfter)
	}
}

func TestSimulatedIOAdvancesWhileRunning(t *testing.T) {
	s := NewSimulatedIO()
	defer s.Close()
	s.SetRunning(true)

	hyd1Before, _ := s.ReadPressureBar(Hyd1Pressure)
	outBefore, _ := s.ReadPressureBar(OutputPressure)
	tankBefore, _ := s.ReadPressureBar(TankPressure)

	for i := 0; i < 20; i++ {
		s.step()
	}

	hyd1After, _ := s.ReadPressureBar(Hyd1Pressure)
	outAfter, _ := s.ReadPressureBar(OutputPressure)
	tankAfter, _ := s.ReadPressureBar(TankPressure)

	if hyd1After <= hyd1Before {
		t.Errorf("Hyd1Pressure should climb while running: %v -> %v", hyd1Before, hyd1After)
	}
	if outAfter <= outBefore {
		t.Errorf("OutputPressure should climb while running: %v -> %v", outBefore, outAfter)
	}
	if tankAfter >= tankBefore {
		t.Errorf("TankPressure should drain while running: %v -> %v", tankBefore, tankAfter)
	}
}

// TestSimulatedIOOutputPressureReleasesWhenNotRunning covers the
// reported request: a real 700 bar fill already brings output pressure
// back down because Controller genuinely opens the vent valve; 350 bar
// has no such real step, so this release is simulated directly whenever
// a fill isn't running - independent of the vent valve's relay state,
// which is why this test deliberately leaves VentValve closed.
func TestSimulatedIOOutputPressureReleasesWhenNotRunning(t *testing.T) {
	s := NewSimulatedIO()
	defer s.Close()
	s.SetRunning(true)
	for i := 0; i < 50; i++ {
		s.step() // climb to a realistic delivery pressure first
	}
	s.SetRunning(false)

	before, _ := s.ReadPressureBar(OutputPressure)
	if before <= 0 {
		t.Fatalf("expected a positive output pressure before release, got %v", before)
	}

	for i := 0; i < 100; i++ {
		s.step()
	}
	after, _ := s.ReadPressureBar(OutputPressure)

	if after != 0 {
		t.Errorf("OutputPressure should release fully back to 0 once not running, got %v", after)
	}
}

// TestSimulatedIOOutputPressureStaysAtZeroWhenIdle: once released, it
// should not go negative or otherwise misbehave on further idle ticks.
func TestSimulatedIOOutputPressureStaysAtZeroWhenIdle(t *testing.T) {
	s := NewSimulatedIO()
	defer s.Close()

	for i := 0; i < 20; i++ {
		s.step()
	}
	after, _ := s.ReadPressureBar(OutputPressure)
	if after != 0 {
		t.Errorf("OutputPressure = %v, want 0", after)
	}
}
