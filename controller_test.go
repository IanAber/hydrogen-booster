package main

import (
	"testing"
	"time"
)

func TestIsValidCustomerCode(t *testing.T) {
	cases := []struct {
		code string
		want bool
	}{
		{"", false},
		{"1", true},
		{"1234567890", true},   // exactly 10 digits
		{"12345678901", false}, // 11 digits, one too many
		{"12a4", false},
		{"123 456", false},
		{"-123", false},
	}
	for _, c := range cases {
		if got := isValidCustomerCode(c.code); got != c.want {
			t.Errorf("isValidCustomerCode(%q) = %v, want %v", c.code, got, c.want)
		}
	}
}

func TestStartAcceptsEmptyCustomerCode(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	if err := c.Start(JobRequest{TargetPressureBar: 100, TargetMassKg: 1, Nozzle: Nozzle350}); err != nil {
		t.Fatalf("expected an empty customer code to be accepted (optional off the operator screen), got %v", err)
	}
}

func TestStartAcceptsValidCustomerCode(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	err := c.Start(JobRequest{TargetPressureBar: 100, TargetMassKg: 1, Nozzle: Nozzle350, CustomerCode: "5551234567"})
	if err != nil {
		t.Fatalf("expected a valid 10-digit customer code to be accepted, got %v", err)
	}
}

func TestStartRejectsMalformedCustomerCode(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	if err := c.Start(JobRequest{TargetPressureBar: 100, TargetMassKg: 1, Nozzle: Nozzle350, CustomerCode: "12a4"}); err == nil {
		t.Error("expected an error for a non-digit customer code")
	}
}

// TestDeliveredMassStopsAfterJobCompletes reproduces the reported bug:
// the dispensed-amount gauge kept creeping up after a fill reported
// Complete, because the simulated tank pressure (which delivered mass is
// derived from) drained every tick regardless of job state. Fixed by
// gating SimulatedIO's physics on Controller telling it whether a job is
// actually running (see RunnableIO in io.go).
func TestDeliveredMassStopsAfterJobCompletes(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	// A low target pressure so the fill completes quickly on the
	// pressure condition (output pressure climbs while running) rather
	// than needing to wait on mass.
	if err := c.Start(JobRequest{TargetPressureBar: 6, TargetMassKg: 999, Nozzle: Nozzle350}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 60; i++ {
		io.step()
	}
	c.superviseJob()
	if got := c.Status().Phase; got != PhaseComplete {
		t.Fatalf("phase = %v, want complete", got)
	}

	// Status() is the reported value the UI polls - it's what should be
	// frozen, not the internal deliveredMassKg() helper (which stays a
	// pure live sensor computation; superviseJob's own target-reached
	// check above needs that).
	massAtComplete := c.Status().DeliveredMassKg
	if massAtComplete <= 0 {
		t.Fatal("expected some delivered mass to have accumulated during the fill")
	}

	// The bug: many more ticks after completion would keep draining the
	// simulated tank (and thus the reported delivered mass) even though
	// the job is no longer running - and separately, even with that
	// frozen, ordinary sensor noise (temperature jitter, present on
	// real hardware too) would still leak through if Status() kept
	// recomputing live instead of reporting the frozen snapshot.
	for i := 0; i < 50; i++ {
		io.step()
	}
	massAfterIdle := c.Status().DeliveredMassKg

	if massAfterIdle != massAtComplete {
		t.Errorf("reported delivered mass drifted after completion: %v -> %v", massAtComplete, massAfterIdle)
	}
}

// TestDeliveredMassFreezesOnOperatorStop mirrors
// TestDeliveredMassStopsAfterJobCompletes for the abort path (Stop),
// not just the normal target-reached completion path.
func TestDeliveredMassFreezesOnOperatorStop(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	if err := c.Start(JobRequest{TargetPressureBar: 350, TargetMassKg: 999, Nozzle: Nozzle350}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		io.step()
	}

	c.Stop()
	if got := c.Status().Phase; got != PhaseIdle {
		t.Fatalf("phase = %v, want idle", got)
	}
	massAtStop := c.Status().DeliveredMassKg

	for i := 0; i < 50; i++ {
		io.step()
	}
	massAfterIdle := c.Status().DeliveredMassKg

	if massAfterIdle != massAtStop {
		t.Errorf("reported delivered mass drifted after an operator Stop: %v -> %v", massAtStop, massAfterIdle)
	}
}

func TestPumpsOnForWholeFillOffAfterStop(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	if err := c.Start(JobRequest{TargetPressureBar: 350, TargetMassKg: 999, Nozzle: Nozzle350}); err != nil {
		t.Fatal(err)
	}
	if on, _ := io.RelayState(Pump1); !on {
		t.Error("Pump1 should be on once a job has started")
	}
	if on, _ := io.RelayState(Pump2); !on {
		t.Error("Pump2 should be on once a job has started")
	}

	c.Stop()
	if on, _ := io.RelayState(Pump1); on {
		t.Error("Pump1 should be off after Stop")
	}
	if on, _ := io.RelayState(Pump2); on {
		t.Error("Pump2 should be off after Stop")
	}
}

// TestNoSpuriousFaultWhileIdle reproduces the other half of the reported
// bug: the customer screen showing a Fault phase before any job had ever
// run, because simulated hydraulic pressure climbed unconditionally from
// boot and eventually tripped the safety limit on its own.
func TestNoSpuriousFaultWhileIdle(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	// Many more idle ticks than it would take the old, ungated physics
	// to trip a hydraulic-pressure fault (roughly 4-5 ticks' worth).
	for i := 0; i < 5000; i++ {
		io.step()
	}
	c.superviseSafety()

	if got := c.Status(); got.Phase == PhaseFault {
		t.Errorf("phase = fault after a long idle period with no job ever started; faults: %v", got.Faults)
	}
}

func TestOutputPressureFilterRejectsIsolatedSpike(t *testing.T) {
	cfg := DefaultConfig()
	cfg.OutputPressureFilterSamples = 3
	live := NewLiveConfig(cfg)
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	steps := []float64{10, 10, 40, 10, 10} // an isolated spike to 40 amid a steady 10
	var got []float64
	for _, v := range steps {
		io.outP = v
		c.superviseJob()
		p, err := c.outputPressureBar()
		if err != nil {
			t.Fatalf("outputPressureBar error: %v", err)
		}
		got = append(got, p)
	}
	if got[2] != 10 {
		t.Fatalf("filtered readings = %v, want the isolated spike at index 2 rejected (median stays 10)", got)
	}
}

func TestOutputPressureFilterDisabledByDefaultPassesThroughRaw(t *testing.T) {
	live := NewLiveConfig(DefaultConfig()) // OutputPressureFilterSamples: 1 (disabled)
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	io.outP = 123.4
	c.superviseJob()
	got, err := c.outputPressureBar()
	if err != nil {
		t.Fatalf("outputPressureBar error: %v", err)
	}
	if got != 123.4 {
		t.Fatalf("outputPressureBar = %v, want 123.4 (raw passthrough - filtering is a no-op at the default)", got)
	}
}

// TestSuperviseSafetyChecksRawOutputPressureEvenWhenFiltered confirms
// the deliberate design choice documented on
// Config.OutputPressureFilterSamples: the MaxHydrogenPressureBar safety
// check must never be masked by output pressure filtering, the same
// principle MaxStrokeTime already relies on for the hydraulic pressure
// filters.
func TestSuperviseSafetyChecksRawOutputPressureEvenWhenFiltered(t *testing.T) {
	cfg := DefaultConfig()
	cfg.OutputPressureFilterSamples = 5 // aggressive filtering, would mask a single-sample spike
	cfg.Safety.MaxHydrogenPressureBar = 50
	live := NewLiveConfig(cfg)
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	// Prime the filter buffer with several safe readings.
	io.outP = 10
	for i := 0; i < 4; i++ {
		c.superviseJob()
	}
	if got := c.Status().Phase; got == PhaseFault {
		t.Fatal("phase = fault before any overpressure reading")
	}

	// A single genuine overpressure sample - the median filter (a
	// 5-sample window, 4 already primed at 10) would still report ~10
	// for it, but the safety check must trip on the raw value anyway.
	io.outP = 999
	c.superviseSafety()

	if got := c.Status().Phase; got != PhaseFault {
		t.Fatalf("phase = %v, want fault - the safety check must never be masked by output pressure filtering", got)
	}
}

func TestSuperviseSafetyFaultsOnLowHydraulicOil(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	io.digitalInputs[HydraulicOilLevel] = true // high/1 = insufficient oil
	c.superviseSafety()

	if got := c.Status().Phase; got != PhaseFault {
		t.Fatalf("phase = %v, want fault", got)
	}
}

func TestSuperviseSafetyFaultsOnMotorNotConfirmedRunning(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	// Pump1 commanded on (as Start() would), but Motor1Powered reads high
	// instead of the expected low (active-low feedback) - the motor
	// never confirmed running.
	_ = io.SetRelay(Pump1, true)
	io.digitalInputs[Motor1Powered] = true
	c.superviseSafety()

	if got := c.Status().Phase; got != PhaseFault {
		t.Fatalf("phase = %v, want fault", got)
	}
}

func TestSuperviseSafetyFaultsOnMotorNotConfirmedStoppedWhilePumpOff(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	// Pump1 never commanded on, but Motor1Powered reads low (instead of
	// the expected high) - the motor never confirmed stopped.
	io.digitalInputs[Motor1Powered] = false
	c.superviseSafety()

	if got := c.Status().Phase; got != PhaseFault {
		t.Fatalf("phase = %v, want fault", got)
	}
}

func TestSuperviseSafetyNoFaultWhenMotorConfirmsRunning(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	// Pump1 commanded on, Motor1Powered correctly reads low - no fault.
	_ = io.SetRelay(Pump1, true)
	io.digitalInputs[Motor1Powered] = false
	c.superviseSafety()

	if got := c.Status().Phase; got == PhaseFault {
		t.Fatalf("phase = fault, want idle (motor confirmed running); faults: %v", c.Status().Faults)
	}
}

func TestSuperviseSafetyNoFaultWhenMotorConfirmsStoppedWhilePumpOff(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	// Pump1 never commanded on, Motor1Powered correctly reads high - no fault.
	io.digitalInputs[Motor1Powered] = true
	c.superviseSafety()

	if got := c.Status().Phase; got == PhaseFault {
		t.Fatalf("phase = fault, want idle (motor confirmed stopped); faults: %v", c.Status().Faults)
	}
}

func TestStartRejectsCodeNotOnAllowList(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	v, err := LoadValidCodes(t.TempDir() + "/valid-codes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.SetAll([]CodeEntry{{Code: "5551234567", Name: "Jane Doe"}}); err != nil {
		t.Fatal(err)
	}
	c.SetValidCodes(v)

	if err := c.Start(JobRequest{TargetPressureBar: 100, TargetMassKg: 1, Nozzle: Nozzle350, CustomerCode: "9999999999"}); err == nil {
		t.Error("expected an error for a code not on the allow-list")
	}
	if err := c.Start(JobRequest{TargetPressureBar: 100, TargetMassKg: 1, Nozzle: Nozzle350, CustomerCode: "5551234567"}); err != nil {
		t.Errorf("expected the allow-listed code to be accepted, got %v", err)
	}
}

func TestStartAllowsEmptyCodeEvenWithAllowListConfigured(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	v, err := LoadValidCodes(t.TempDir() + "/valid-codes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.SetAll([]CodeEntry{{Code: "5551234567", Name: "Jane Doe"}}); err != nil {
		t.Fatal(err)
	}
	c.SetValidCodes(v)

	// The staff operator screen has no code field at all and must keep
	// working even once an allow-list is configured for the public one.
	if err := c.Start(JobRequest{TargetPressureBar: 100, TargetMassKg: 1, Nozzle: Nozzle350}); err != nil {
		t.Errorf("expected an empty customer code to bypass the allow-list, got %v", err)
	}
}

func TestCompletedJobIsRecordedInDispenseLog(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MassMeasurementEnabled = true
	live := NewLiveConfig(cfg)
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	d := NewDispenseLog(t.TempDir() + "/dispense-log.jsonl")
	c.SetDispenseLog(d)

	if err := c.Start(JobRequest{TargetPressureBar: 6, TargetMassKg: 999, Nozzle: Nozzle350, CustomerCode: "5551234567"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		io.step()
	}
	c.superviseJob()
	if got := c.Status().Phase; got != PhaseComplete {
		t.Fatalf("phase = %v, want complete", got)
	}

	records, err := d.DrainAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].CustomerCode != "5551234567" {
		t.Errorf("CustomerCode = %q, want 5551234567", records[0].CustomerCode)
	}
	if records[0].DeliveredMassKg <= 0 {
		t.Errorf("DeliveredMassKg = %v, want > 0", records[0].DeliveredMassKg)
	}
}

// TestCompletedJobIsNotRecordedWhenMassMeasurementDisabled confirms
// the default (MassMeasurementEnabled: false, per DefaultConfig)
// never writes an estimated delivered-mass figure to the dispense log
// - it's a tank pressure differential estimate, not a scale reading,
// and shouldn't be handed to a billing/accounting system as fact.
func TestCompletedJobIsNotRecordedWhenMassMeasurementDisabled(t *testing.T) {
	live := NewLiveConfig(DefaultConfig()) // MassMeasurementEnabled: false
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	d := NewDispenseLog(t.TempDir() + "/dispense-log.jsonl")
	c.SetDispenseLog(d)

	if err := c.Start(JobRequest{TargetPressureBar: 6, TargetMassKg: 999, Nozzle: Nozzle350}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		io.step()
	}
	c.superviseJob()
	if got := c.Status().Phase; got != PhaseComplete {
		t.Fatalf("phase = %v, want complete", got)
	}

	records, err := d.DrainAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("got %d records, want 0 (mass measurement disabled)", len(records))
	}
}

func TestAbortedJobIsNotRecorded(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	d := NewDispenseLog(t.TempDir() + "/dispense-log.jsonl")
	c.SetDispenseLog(d)

	if err := c.Start(JobRequest{TargetPressureBar: 350, TargetMassKg: 999, Nozzle: Nozzle350, CustomerCode: "5551234567"}); err != nil {
		t.Fatal(err)
	}
	c.Stop()

	records, err := d.DrainAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Errorf("expected an operator-aborted job not to be recorded, got %v", records)
	}
}

func TestTickFansVentFanEnergisesWithEitherPump(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	_ = io.SetRelay(Pump1, true)
	c.tickFans(time.Now())

	if on, _ := io.RelayState(VentFan); !on {
		t.Fatal("VentFan should energise as soon as either pump is commanded on")
	}
}

func TestTickFansVentFanStaysOnDuringOffDelayThenDeenergises(t *testing.T) {
	cfg := DefaultConfig()
	cfg.VentFan.OffDelay = 5 * time.Second
	live := NewLiveConfig(cfg)
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	_ = io.SetRelay(Pump1, true)
	start := time.Now()
	c.tickFans(start)

	_ = io.SetRelay(Pump1, false)
	c.tickFans(start.Add(1 * time.Second))
	if on, _ := io.RelayState(VentFan); !on {
		t.Fatal("VentFan should still be energised - within the configured off delay")
	}

	c.tickFans(start.Add(6 * time.Second))
	if on, _ := io.RelayState(VentFan); on {
		t.Fatal("VentFan should have de-energised once the off delay elapsed")
	}
}

func TestTickFansCoolingFanStaysOffBelowSetpoint(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CoolingFan.SetpointC = 50
	live := NewLiveConfig(cfg)
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	_ = io.SetRelay(Pump1, true)
	io.temps[HydOilTemp] = 40
	c.tickFans(time.Now())

	if on, _ := io.RelayState(CoolingFan); on {
		t.Fatal("CoolingFan should stay off while oil is below the setpoint")
	}
}

func TestTickFansCoolingFanTurnsOnAboveSetpointWhilePumpsRun(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CoolingFan.SetpointC = 50
	live := NewLiveConfig(cfg)
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	_ = io.SetRelay(Pump1, true)
	io.temps[HydOilTemp] = 55
	c.tickFans(time.Now())

	if on, _ := io.RelayState(CoolingFan); !on {
		t.Fatal("CoolingFan should turn on once oil exceeds the setpoint while the pumps are running")
	}
}

func TestTickFansCoolingFanDoesNotTurnOnAboveSetpointWhilePumpsOff(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CoolingFan.SetpointC = 50
	live := NewLiveConfig(cfg)
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	// Pumps never commanded on.
	io.temps[HydOilTemp] = 55
	c.tickFans(time.Now())

	if on, _ := io.RelayState(CoolingFan); on {
		t.Fatal("CoolingFan should not turn on from temperature alone while the pumps aren't running")
	}
}

func TestTickFansCoolingFanStaysOnAfterPumpsStopUntilBelowHysteresis(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CoolingFan.SetpointC = 50
	live := NewLiveConfig(cfg)
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	_ = io.SetRelay(Pump1, true)
	io.temps[HydOilTemp] = 55
	c.tickFans(time.Now())
	if on, _ := io.RelayState(CoolingFan); !on {
		t.Fatal("CoolingFan should have turned on")
	}

	// Pumps stop, oil still above setpoint - fan should stay on.
	_ = io.SetRelay(Pump1, false)
	c.tickFans(time.Now())
	if on, _ := io.RelayState(CoolingFan); !on {
		t.Fatal("CoolingFan should stay on after the pumps stop while oil is still above setpoint")
	}

	// Oil drops to just below the setpoint, but not below setpoint-1 - still on.
	io.temps[HydOilTemp] = 49.5
	c.tickFans(time.Now())
	if on, _ := io.RelayState(CoolingFan); !on {
		t.Fatal("CoolingFan should stay on until oil drops below setpoint-1, not merely below setpoint")
	}

	// Oil drops below setpoint-1 - now it should turn off.
	io.temps[HydOilTemp] = 48.9
	c.tickFans(time.Now())
	if on, _ := io.RelayState(CoolingFan); on {
		t.Fatal("CoolingFan should turn off once oil drops below setpoint-1")
	}
}

// fakeFlowMeterIO wraps a full IO implementation (SimulatedIO in these
// tests) and adds the optional FlowMeterIO capability, so
// Controller.deliveredMassKg()'s preference order (real flow meter over
// the tank pressure-differential estimate) can be tested without a real
// CompositeIO/FlowModule - see FlowMeterIO in io.go.
type fakeFlowMeterIO struct {
	IO
	totalKg    float64
	err        error
	resetCalls int
}

func (f *fakeFlowMeterIO) ReadFlowTotalKg() (float64, error) { return f.totalKg, f.err }
func (f *fakeFlowMeterIO) ResetFlowTotal() error {
	f.resetCalls++
	return nil
}

func TestDeliveredMassPrefersFlowMeterWhenAvailable(t *testing.T) {
	base := NewSimulatedIO()
	defer base.Close()
	fake := &fakeFlowMeterIO{IO: base, totalKg: 2.5}
	live := NewLiveConfig(DefaultConfig())
	c := NewController(live, fake)

	if err := c.Start(JobRequest{TargetPressureBar: 700, TargetMassKg: 999, Nozzle: Nozzle700}); err != nil {
		t.Fatal(err)
	}
	if got := c.Status().DeliveredMassKg; got != 2.5 {
		t.Fatalf("DeliveredMassKg = %v, want 2.5 (from the flow meter)", got)
	}
}

func TestDeliveredMassFallsBackToTankEstimateWhenFlowMeterErrors(t *testing.T) {
	base := NewSimulatedIO()
	defer base.Close()
	fake := &fakeFlowMeterIO{IO: base, err: ErrSensorFault}
	live := NewLiveConfig(DefaultConfig())
	c := NewController(live, fake)

	if err := c.Start(JobRequest{TargetPressureBar: 700, TargetMassKg: 999, Nozzle: Nozzle700}); err != nil {
		t.Fatal(err)
	}
	// Should fall back to the tank estimate rather than propagate the
	// flow meter's error or panic - a non-negative value (likely ~0
	// immediately after Start) confirms the fallback path ran.
	if got := c.Status().DeliveredMassKg; got < 0 {
		t.Fatalf("DeliveredMassKg = %v, want >= 0 (tank estimate fallback)", got)
	}
}

func TestStartResetsFlowTotalizer(t *testing.T) {
	base := NewSimulatedIO()
	defer base.Close()
	fake := &fakeFlowMeterIO{IO: base}
	live := NewLiveConfig(DefaultConfig())
	c := NewController(live, fake)

	if err := c.Start(JobRequest{TargetPressureBar: 700, TargetMassKg: 999, Nozzle: Nozzle700}); err != nil {
		t.Fatal(err)
	}
	if fake.resetCalls != 1 {
		t.Fatalf("ResetFlowTotal called %d times, want 1", fake.resetCalls)
	}
}

func TestSetManualModeEnableRejectedWhileFilling(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	if err := c.Start(JobRequest{TargetPressureBar: 700, TargetMassKg: 999, Nozzle: Nozzle700}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetManualMode(true); err == nil {
		t.Error("expected enabling manual mode to be rejected while a job is running")
	}
}

func TestStartRejectedWhileManualModeEnabled(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	if err := c.SetManualMode(true); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(JobRequest{TargetPressureBar: 100, TargetMassKg: 1, Nozzle: Nozzle350}); err == nil {
		t.Error("expected Start to be rejected while manual mode is enabled")
	}
}

// TestSetManualModeDefaultsRelaysSafe confirms both the stage relief
// valves (B/D) come up open and the direction valves/pumps come up off
// on entering manual mode - the required manual-page starting state -
// even if they were left in some other state by a prior job or fault.
func TestSetManualModeDefaultsRelaysSafe(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	_ = io.SetRelay(Valve1A, true)
	_ = io.SetRelay(Valve1B, false)
	_ = io.SetRelay(Valve2A, true)
	_ = io.SetRelay(Valve2B, false)
	_ = io.SetRelay(Pump1, true)
	_ = io.SetRelay(Pump2, true)

	if err := c.SetManualMode(true); err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		relay RelayOutput
		want  bool
	}{
		{Valve1A, false}, {Valve1B, true},
		{Valve2A, false}, {Valve2B, true},
		{Pump1, false}, {Pump2, false},
	}
	for _, chk := range checks {
		got, err := io.RelayState(chk.relay)
		if err != nil {
			t.Fatalf("RelayState(%v): %v", chk.relay, err)
		}
		if got != chk.want {
			t.Errorf("relay %v = %v, want %v", chk.relay, got, chk.want)
		}
	}
}

func TestManualSetPumpRejectedOutsideManualMode(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	if err := c.ManualSetPump(1, true); err == nil {
		t.Error("expected ManualSetPump to be rejected while manual mode is disabled")
	}
}

func TestManualSetPumpTogglesRequestedStage(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)
	if err := c.SetManualMode(true); err != nil {
		t.Fatal(err)
	}

	if err := c.ManualSetPump(2, true); err != nil {
		t.Fatal(err)
	}
	if on, _ := io.RelayState(Pump2); !on {
		t.Error("Pump2 should be on")
	}
	if on, _ := io.RelayState(Pump1); on {
		t.Error("Pump1 should remain off")
	}
}

func TestManualSetPumpRejectsInvalidStage(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)
	_ = c.SetManualMode(true)

	if err := c.ManualSetPump(3, true); err == nil {
		t.Error("expected an error for an invalid stage number")
	}
}

func TestManualJogRejectedOutsideManualMode(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)

	if err := c.ManualJog(1, true, true); err == nil {
		t.Error("expected ManualJog to be rejected while manual mode is disabled")
	}
}

// TestManualJogSameDirectionClosesReliefImmediately confirms that
// pressing a jog button for the direction the valve is already in
// (stage 2 defaults to "down" on entering manual mode - see
// SetManualMode) closes the relief valve straight away, with no
// reverse-delay wait, since the direction valve needs no time to
// settle - while leaving the other stage untouched.
func TestManualJogSameDirectionClosesReliefImmediately(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)
	_ = c.SetManualMode(true)

	if err := c.ManualJog(2, false, true); err != nil {
		t.Fatal(err)
	}
	if dir, _ := io.RelayState(Valve2A); dir {
		t.Error("Valve2A (stage 2 direction) should remain de-energised (down)")
	}
	if relief, _ := io.RelayState(Valve2B); relief {
		t.Error("Valve2B (stage 2 relief) should be closed immediately - no direction change to settle")
	}
	if relief, _ := io.RelayState(Valve1B); !relief {
		t.Error("stage 1's relief valve should be untouched (still open)")
	}
}

// TestManualJogReleaseReopensRelief confirms releasing the button
// re-opens the relief valve regardless of which direction was held.
func TestManualJogReleaseReopensRelief(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)
	_ = c.SetManualMode(true)

	if err := c.ManualJog(2, false, true); err != nil {
		t.Fatal(err)
	}
	if relief, _ := io.RelayState(Valve2B); relief {
		t.Fatal("relief should be closed while held")
	}
	if err := c.ManualJog(2, false, false); err != nil {
		t.Fatal(err)
	}
	if relief, _ := io.RelayState(Valve2B); !relief {
		t.Error("Valve2B (stage 2 relief) should re-open on release")
	}
}

// TestManualJogDirectionChangeKeepsReliefOpenUntilReverseDelay confirms
// that pressing a jog button for the OTHER direction (stage 1 defaults
// to "down" on entering manual mode) sets the direction valve right
// away but leaves the relief valve open until ReverseDelay has elapsed
// - mirroring Stages.tickReversing's settling time for the automatic
// sequencer - closing it only once tickManualJog sees the deadline has
// passed.
func TestManualJogDirectionChangeKeepsReliefOpenUntilReverseDelay(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)
	_ = c.SetManualMode(true)
	reverseDelay := live.Snapshot().Stage1.ReverseDelay

	t0 := time.Now()
	if err := c.manualJogAt(1, true, true, t0); err != nil {
		t.Fatal(err)
	}
	if dir, _ := io.RelayState(Valve1A); !dir {
		t.Error("Valve1A (stage 1 direction) should be energised (up) immediately")
	}
	if relief, _ := io.RelayState(Valve1B); !relief {
		t.Error("Valve1B (stage 1 relief) should still be open right after the direction change")
	}

	c.tickManualJog(t0.Add(reverseDelay - time.Millisecond))
	if relief, _ := io.RelayState(Valve1B); !relief {
		t.Error("Valve1B should still be open just before ReverseDelay elapses")
	}

	c.tickManualJog(t0.Add(reverseDelay + time.Millisecond))
	if relief, _ := io.RelayState(Valve1B); relief {
		t.Error("Valve1B should close once ReverseDelay has elapsed")
	}
}

// TestManualJogReleaseBeforeReverseDelayCancelsPendingClose confirms
// that releasing a jog button that changed direction - before
// ReverseDelay elapses - cancels the pending close: a later
// tickManualJog call for that deadline must not close the relief valve
// out from under a press that already ended.
func TestManualJogReleaseBeforeReverseDelayCancelsPendingClose(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)
	_ = c.SetManualMode(true)
	reverseDelay := live.Snapshot().Stage1.ReverseDelay

	t0 := time.Now()
	if err := c.manualJogAt(1, true, true, t0); err != nil {
		t.Fatal(err)
	}
	if err := c.ManualJog(1, true, false); err != nil {
		t.Fatal(err)
	}
	if relief, _ := io.RelayState(Valve1B); !relief {
		t.Fatal("relief should have re-opened on release")
	}

	// The deadline from the original press, if still pending, would
	// fall due right about now - confirm it doesn't fire.
	c.tickManualJog(t0.Add(reverseDelay + time.Millisecond))
	if relief, _ := io.RelayState(Valve1B); !relief {
		t.Error("a cancelled reverse-delay close must not close the relief valve after release")
	}
}

// TestSetManualModeClearsPendingReverseDelay confirms that leaving
// manual mode while a reverse-delay close is pending (button still held
// when Disable is clicked) discards it - tickManualJog must not re-close
// the relief valve SetManualMode just forced open.
func TestSetManualModeClearsPendingReverseDelay(t *testing.T) {
	live := NewLiveConfig(DefaultConfig())
	io := NewSimulatedIO()
	defer io.Close()
	c := NewController(live, io)
	_ = c.SetManualMode(true)
	reverseDelay := live.Snapshot().Stage1.ReverseDelay

	t0 := time.Now()
	if err := c.manualJogAt(1, true, true, t0); err != nil {
		t.Fatal(err)
	}
	if err := c.SetManualMode(false); err != nil {
		t.Fatal(err)
	}

	c.tickManualJog(t0.Add(reverseDelay + time.Millisecond))
	if relief, _ := io.RelayState(Valve1B); !relief {
		t.Error("Valve1B should remain open - SetManualMode's safe default, not re-closed by a stale pending reversal")
	}
}
