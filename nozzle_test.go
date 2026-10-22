package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func newNozzleController(t *testing.T, has700, sensors bool) (*Controller, *SimulatedIO) {
	cfg := DefaultConfig()
	cfg.Nozzles = NozzleConfig{Has700Bar: has700, SensorsEnabled: sensors}
	io := NewSimulatedIO()
	t.Cleanup(func() { io.Close() })
	return NewController(NewLiveConfig(cfg), io), io
}

func setLifted(io *SimulatedIO, n350, n700 bool) {
	io.mu.Lock()
	io.digitalInputs[Nozzle350InUse] = n350
	io.digitalInputs[Nozzle700InUse] = n700
	io.mu.Unlock()
}

func errCode(err error) string { return messageOf(err).Code }

func TestStartRejects700WhenNotFitted(t *testing.T) {
	c, _ := newNozzleController(t, false, false)
	err := c.Start(JobRequest{TargetPressureBar: 700, TargetMassKg: 1, Nozzle: Nozzle700})
	if errCode(err) != "nozzle_700_not_fitted" {
		t.Fatalf("err = %v, want nozzle_700_not_fitted", err)
	}
	if err := c.Start(JobRequest{TargetPressureBar: 351, TargetMassKg: 1, Nozzle: Nozzle350}); errCode(err) != "target_pressure_range" {
		t.Fatalf("err = %v, want the 350 bar cap", err)
	}
	if err := c.Start(JobRequest{TargetPressureBar: 350, TargetMassKg: 1, Nozzle: Nozzle350}); err != nil {
		t.Fatalf("350 bar fill should start: %v", err)
	}
}

func TestNozzleSensorsSelectTheLiftedNozzle(t *testing.T) {
	c, io := newNozzleController(t, true, true)

	setLifted(io, false, false)
	if err := c.Start(JobRequest{TargetPressureBar: 350, TargetMassKg: 1}); errCode(err) != "nozzle_none_lifted" {
		t.Fatalf("none lifted: err = %v", err)
	}
	setLifted(io, true, true)
	if err := c.Start(JobRequest{TargetPressureBar: 350, TargetMassKg: 1}); errCode(err) != "nozzle_both_lifted" {
		t.Fatalf("both lifted: err = %v", err)
	}
	setLifted(io, false, true)
	if err := c.Start(JobRequest{TargetPressureBar: 350, TargetMassKg: 1, Nozzle: Nozzle350}); errCode(err) != "nozzle_mismatch" {
		t.Fatalf("request for the other nozzle: err = %v", err)
	}
	if st := c.Status().Nozzles; st.Detected != 700 || st.Problem != nil {
		t.Fatalf("status = %+v, want 700 detected", st)
	}
	if err := c.Start(JobRequest{TargetPressureBar: 700, TargetMassKg: 1}); err != nil {
		t.Fatalf("700 lifted: %v", err)
	}
	if got := c.job.Nozzle; got != Nozzle700 {
		t.Errorf("job nozzle = %v, want 700 from the sensor", got)
	}
}

func TestNozzleSensorsIgnore700WhenNotFitted(t *testing.T) {
	c, io := newNozzleController(t, false, true)
	setLifted(io, true, true) // 700 input unwired/floating - must be ignored
	if err := c.Start(JobRequest{TargetPressureBar: 350, TargetMassKg: 1}); err != nil {
		t.Fatalf("350 lifted on a 350-only system: %v", err)
	}
}

func TestNozzleReturnedMidFillStopsTheJob(t *testing.T) {
	c, io := newNozzleController(t, true, true)
	setLifted(io, true, false)
	if err := c.Start(JobRequest{TargetPressureBar: 350, TargetMassKg: 999}); err != nil {
		t.Fatal(err)
	}
	setLifted(io, false, false)
	c.superviseJob()

	st := c.Status()
	if st.Phase != PhaseFault || len(st.FaultDetails) == 0 || !strings.Contains(st.Faults[0], "350 bar nozzle is no longer") {
		t.Fatalf("phase=%v faults=%v, want a nozzle fault", st.Phase, st.Faults)
	}
	if on, _ := io.RelayState(Pump1); on {
		t.Error("pumps should be stopped")
	}
}

func TestNozzleSensorsUnreadableOnlyMatterWhenEnabled(t *testing.T) {
	sim := NewSimulatedIO()
	t.Cleanup(func() { sim.Close() })
	io := &sensorFaultIO{SimulatedIO: sim, badDigital: map[DigitalInput]error{Nozzle700InUse: errors.New("GPIO read failed")}}

	cfg := DefaultConfig() // sensors disabled
	c := NewController(NewLiveConfig(cfg), io)
	c.superviseSafety()
	if c.Status().Phase == PhaseFault {
		t.Fatal("an unused nozzle sensor must not fault the system")
	}

	cfg.Nozzles.SensorsEnabled = true
	c = NewController(NewLiveConfig(cfg), io)
	c.superviseSafety()
	if c.Status().Phase != PhaseFault {
		t.Fatal("an unreadable nozzle sensor should fault the system once sensors are enabled")
	}
}

func TestNozzleConfigDefaultsAndValidation(t *testing.T) {
	// A config file from before this setting existed keeps 700 bar on.
	path := t.TempDir() + "/config.json"
	dto := ToDTO(DefaultConfig())
	raw, _ := json.Marshal(dto)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	delete(m, "nozzles")
	raw, _ = json.Marshal(m)
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := LoadConfigFile(path, DefaultConfig())
	if err != nil || !ok || !loaded.Nozzles.Has700Bar || loaded.Nozzles.SensorsEnabled {
		t.Fatalf("loaded nozzles = %+v (ok=%v err=%v), want 700 bar on, sensors off", loaded.Nozzles, ok, err)
	}

	// Enabling sensors requires them to be assigned.
	dto = ToDTO(DefaultConfig())
	dto.Nozzles.SensorsEnabled = true
	dto.DigitalInputModule.Channels[5].Enabled = false // Nozzle700InUse
	if _, err := FromDTO(DefaultConfig(), dto); err == nil || !strings.Contains(err.Error(), "Nozzle700InUse") {
		t.Fatalf("err = %v, want Nozzle700InUse unassigned", err)
	}
	f := false
	dto.Nozzles.Has700Bar = &f // ...but not on a 350-only system
	if _, err := FromDTO(DefaultConfig(), dto); err != nil {
		t.Fatalf("350-only system shouldn't need the 700 sensor: %v", err)
	}
}
