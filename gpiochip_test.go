package main

import (
	"errors"
	"testing"
)

// fakeGpioLines is an in-memory GpioLines for testing RelayModule without
// any real hardware or Linux ioctls.
type fakeGpioLines struct {
	bits      uint64
	closed    bool
	setErr    error
	getErr    error
	setCalls  int
	closeSeen bool
}

func (f *fakeGpioLines) SetValues(mask, bits uint64) error {
	f.setCalls++
	if f.setErr != nil {
		return f.setErr
	}
	f.bits = (f.bits &^ mask) | (bits & mask)
	return nil
}

func (f *fakeGpioLines) GetValues() (uint64, error) {
	if f.getErr != nil {
		return 0, f.getErr
	}
	return f.bits, nil
}

func (f *fakeGpioLines) Close() error {
	f.closed = true
	f.closeSeen = true
	return nil
}

func fakeOpenFn(lines *fakeGpioLines, offsets *[]int) func(string, []int, string) (GpioLines, error) {
	return func(chip string, off []int, consumer string) (GpioLines, error) {
		*offsets = append([]int{}, off...)
		return lines, nil
	}
}

func TestRelayModuleOpenLinesUsesEnabledChannelsInOrder(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RelayModule.LiveEnabled = true
	cfg.RelayModule.Channels[0] = RelayChannelConfig{Enabled: true, Assigned: Valve1A, Port: 7}
	cfg.RelayModule.Channels[1] = RelayChannelConfig{Enabled: true, Assigned: VentValve, Port: 3}
	cfg.RelayModule.Channels[2] = RelayChannelConfig{Enabled: false, Assigned: Valve1B, Port: 9}
	cfg.RelayModule.Channels[3] = RelayChannelConfig{Enabled: false}
	cfg.RelayModule.Channels[4] = RelayChannelConfig{Enabled: false}
	cfg.RelayModule.Channels[5] = RelayChannelConfig{Enabled: false}
	cfg.RelayModule.Channels[6] = RelayChannelConfig{Enabled: false}
	cfg.RelayModule.Channels[7] = RelayChannelConfig{Enabled: false}
	cfg.RelayModule.Channels[8] = RelayChannelConfig{Enabled: false}
	live := NewLiveConfig(cfg)

	fake := &fakeGpioLines{}
	var openedOffsets []int
	rm := NewRelayModule(live, fakeOpenFn(fake, &openedOffsets))

	if err := rm.openLines(cfg.RelayModule); err != nil {
		t.Fatal(err)
	}
	if len(openedOffsets) != 2 || openedOffsets[0] != 7 || openedOffsets[1] != 3 {
		t.Fatalf("openedOffsets = %v, want [7 3]", openedOffsets)
	}

	// Valve1A -> index 0 (bit 0), VentValve -> index 1 (bit 1).
	handled, err := rm.SetRelay(Valve1A, true)
	if !handled || err != nil {
		t.Fatalf("SetRelay(Valve1A) handled=%v err=%v", handled, err)
	}
	if fake.bits != 0x1 {
		t.Errorf("bits = %#x, want 0x1", fake.bits)
	}

	handled, err = rm.SetRelay(VentValve, true)
	if !handled || err != nil {
		t.Fatalf("SetRelay(VentValve) handled=%v err=%v", handled, err)
	}
	if fake.bits != 0x3 {
		t.Errorf("bits = %#x, want 0x3", fake.bits)
	}

	// Turning Valve1A off must not disturb VentValve's bit.
	handled, err = rm.SetRelay(Valve1A, false)
	if !handled || err != nil {
		t.Fatalf("SetRelay(Valve1A off) handled=%v err=%v", handled, err)
	}
	if fake.bits != 0x2 {
		t.Errorf("bits = %#x, want 0x2 (VentValve should remain on)", fake.bits)
	}

	on, handled, err := rm.RelayState(VentValve)
	if !handled || err != nil || !on {
		t.Fatalf("RelayState(VentValve) = on=%v handled=%v err=%v, want true,true,nil", on, handled, err)
	}

	// A valve not assigned to any enabled channel must report unhandled,
	// so CompositeIO falls back to the simulator.
	if _, handled, _ := rm.RelayState(Valve2A); handled {
		t.Error("Valve2A should not be handled (no enabled channel assigns it)")
	}
}

func TestRelayModuleUnhandledWhenNotLive(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RelayModule.LiveEnabled = false
	live := NewLiveConfig(cfg)

	fake := &fakeGpioLines{}
	var offsets []int
	rm := NewRelayModule(live, fakeOpenFn(fake, &offsets))
	// Never opened - Run() wouldn't call openLines while LiveEnabled=false.

	if handled, err := rm.SetRelay(Valve1A, true); handled || err != nil {
		t.Errorf("SetRelay before any open: handled=%v err=%v, want false,nil", handled, err)
	}
}

func TestRelayModulePropagatesHardwareError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RelayModule.LiveEnabled = true
	cfg.RelayModule.Channels[0] = RelayChannelConfig{Enabled: true, Assigned: Valve1A, Port: 0}
	live := NewLiveConfig(cfg)

	fake := &fakeGpioLines{setErr: errors.New("boom")}
	var offsets []int
	rm := NewRelayModule(live, fakeOpenFn(fake, &offsets))
	if err := rm.openLines(cfg.RelayModule); err != nil {
		t.Fatal(err)
	}

	handled, err := rm.SetRelay(Valve1A, true)
	if !handled {
		t.Error("expected handled=true even on a hardware error, so CompositeIO doesn't double-write")
	}
	if err == nil {
		t.Error("expected the hardware error to propagate")
	}
}

func TestRelayModuleReopensOnConfigChange(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RelayModule.LiveEnabled = true
	cfg.RelayModule.Channels = [RelayChannelCount]RelayChannelConfig{
		{Enabled: true, Assigned: Valve1A, Port: 0},
	}
	live := NewLiveConfig(cfg)

	first := &fakeGpioLines{}
	var firstOffsets []int
	rm := NewRelayModule(live, fakeOpenFn(first, &firstOffsets))
	if err := rm.openLines(cfg.RelayModule); err != nil {
		t.Fatal(err)
	}
	sigBefore := rm.currentSignature()

	// Re-point Valve1A at a different port - the signature must change so
	// Run() knows to reopen rather than keep driving the old port.
	cfg2 := cfg
	cfg2.RelayModule.Channels[0].Port = 5
	if relayConfigSignature(cfg2.RelayModule) == sigBefore {
		t.Fatal("signature did not change after re-mapping a port")
	}

	second := &fakeGpioLines{}
	var secondOffsets []int
	rm.openFn = fakeOpenFn(second, &secondOffsets)
	if err := rm.openLines(cfg2.RelayModule); err != nil {
		t.Fatal(err)
	}
	if !first.closeSeen {
		t.Error("expected the old line request to be closed on reopen")
	}
	if len(secondOffsets) != 1 || secondOffsets[0] != 5 {
		t.Errorf("secondOffsets = %v, want [5]", secondOffsets)
	}
}
