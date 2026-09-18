package main

import "testing"

func TestPollOnceParsesAndCalibratesAgainstConfirmedFrames(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CanModule.LiveEnabled = true
	live := NewLiveConfig(cfg)
	pm := NewPressureModule(live, nil)

	fake := &fakeCanTransport{
		toRecv: []CanFrame{
			// channel0=0, channel1=0x8000 (mid-scale), channel2=0xFFFF (full-scale)
			{ID: cfg.CanModule.ResponseID, Len: 8, Data: [8]byte{0x02, 0x00, 0x00, 0x00, 0x80, 0x00, 0xFF, 0xFF}},
			// channel3=0, channel4=0, channel5=0 (spare/disabled, ignored)
			{ID: cfg.CanModule.ResponseID, Len: 8, Data: [8]byte{0x02, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
		},
	}
	pm.transport = fake
	pm.pollOnce(cfg.CanModule)

	if len(fake.sent) != 1 {
		t.Fatalf("expected exactly 1 request sent, got %d", len(fake.sent))
	}
	if fake.sent[0].ID != 0x101 || fake.sent[0].Data[2] != 0x06 {
		t.Errorf("unexpected request sent: %+v", fake.sent[0])
	}

	// Channel 0 -> Hyd1Pressure, raw 0 -> calibrated 0 bar.
	v, ok := pm.ReadPressureBar(Hyd1Pressure)
	if !ok || v != 0 {
		t.Errorf("Hyd1Pressure = %v, ok=%v; want 0, true", v, ok)
	}

	// Channel 1 -> Hyd2Pressure, raw 0x8000 (~half of 65535) -> ~125 of 0..250 bar.
	v2, ok2 := pm.ReadPressureBar(Hyd2Pressure)
	if !ok2 {
		t.Fatal("Hyd2Pressure not ok")
	}
	if v2 < 124 || v2 > 126 {
		t.Errorf("Hyd2Pressure = %v, want ~125", v2)
	}

	// Channel 2 -> TankPressure, raw 0xFFFF (full scale) -> 300 bar.
	v3, ok3 := pm.ReadPressureBar(TankPressure)
	if !ok3 || v3 != 300 {
		t.Errorf("TankPressure = %v, ok=%v; want 300, true", v3, ok3)
	}

	// A sensor not on this module at all must report not-ok, so
	// CompositeIO knows to fall back to the base simulated IO.
	if _, ok := pm.ReadPressureBar(Hyd1Temp); ok {
		t.Error("Hyd1Temp should not be reported as assigned to the CAN module")
	}

	st := pm.Status()
	if !st.CommsOK {
		t.Errorf("expected CommsOK after a clean poll, lastErr=%q", st.LastError)
	}
}

func TestPollOnceIgnoresFramesWithWrongIDOrFuncCode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CanModule.LiveEnabled = true
	live := NewLiveConfig(cfg)
	pm := NewPressureModule(live, nil)

	fake := &fakeCanTransport{
		toRecv: []CanFrame{
			{ID: 0x999, Len: 8, Data: [8]byte{0x02, 0x00, 0, 0, 0, 0, 0, 0}},                    // wrong ID, ignored
			{ID: cfg.CanModule.ResponseID, Len: 8, Data: [8]byte{0x05, 0x00, 0, 0, 0, 0, 0, 0}}, // wrong func code, ignored
			{ID: cfg.CanModule.ResponseID, Len: 8, Data: [8]byte{0x02, 0x00, 0, 10, 0, 0, 0, 0}},
			{ID: cfg.CanModule.ResponseID, Len: 8, Data: [8]byte{0x02, 0x03, 0, 0, 0, 0, 0, 0}},
		},
	}
	pm.transport = fake
	pm.pollOnce(cfg.CanModule)

	v, ok := pm.ReadPressureBar(Hyd1Pressure)
	if !ok {
		t.Fatal("expected a value despite noise frames")
	}
	// raw=10 on default 0..65535 -> 0..250 bar calibration is ~0.0000... bar, just check it's tiny and non-negative.
	if v < 0 || v > 1 {
		t.Errorf("Hyd1Pressure = %v, want a small value near 0", v)
	}
}

func TestPollOnceTimesOutCleanly(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CanModule.ResponseTimeout = 0 // force immediate timeout, no frames needed
	live := NewLiveConfig(cfg)
	pm := NewPressureModule(live, nil)

	fake := &fakeCanTransport{} // no frames queued
	pm.transport = fake
	pm.pollOnce(cfg.CanModule)

	st := pm.Status()
	if st.CommsOK {
		t.Error("expected CommsOK=false after a timeout with no response frames")
	}
	if st.LastError == "" {
		t.Error("expected a non-empty LastError after a timeout")
	}
}

func TestLiveToSimSnapshotThenAdjustable(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CanModule.LiveEnabled = true
	live := NewLiveConfig(cfg)
	pm := NewPressureModule(live, nil)

	// Simulate a prior live reading.
	pm.barLive[TankPressure] = 42.5

	// Flip to simulation mode - snapshotToSim happens once, exactly as
	// Run() does on the true->false transition.
	c := live.Snapshot()
	c.CanModule.LiveEnabled = false
	live.Set(c)
	pm.snapshotToSim()

	v, ok := pm.ReadPressureBar(TankPressure)
	if !ok || v != 42.5 {
		t.Fatalf("expected the snapshotted 42.5 in sim mode, got %v ok=%v", v, ok)
	}

	if err := pm.SetSimValue(TankPressure, 99); err != nil {
		t.Fatal(err)
	}
	v2, _ := pm.ReadPressureBar(TankPressure)
	if v2 != 99 {
		t.Fatalf("expected the adjusted 99 after SetSimValue, got %v", v2)
	}

	// Setting a sim value while live is a mistake the API should reject.
	c2 := live.Snapshot()
	c2.CanModule.LiveEnabled = true
	live.Set(c2)
	if err := pm.SetSimValue(TankPressure, 1); err == nil {
		t.Error("expected an error setting a simulated value while live is on")
	}
}

func TestSetSimValueRejectsUnassignedSensor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CanModule.LiveEnabled = false
	live := NewLiveConfig(cfg)
	pm := NewPressureModule(live, nil)

	if err := pm.SetSimValue(Hyd1Temp, 20); err == nil {
		t.Error("expected an error setting a sim value for a sensor not assigned to this module")
	}
}
