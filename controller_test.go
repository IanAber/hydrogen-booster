package main

import "testing"

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
	live := NewLiveConfig(DefaultConfig())
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
