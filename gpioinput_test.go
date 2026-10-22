package main

import (
	"errors"
	"fmt"
	"testing"
)

// fakeDigitalInputLines is an in-memory DigitalInputLines for testing
// DigitalInputModule without any real hardware or Linux ioctls.
type fakeDigitalInputLines struct {
	bits      uint64
	closed    bool
	getErr    error
	closeSeen bool
}

func (f *fakeDigitalInputLines) GetValues() (uint64, error) {
	if f.getErr != nil {
		return 0, f.getErr
	}
	return f.bits, nil
}

func (f *fakeDigitalInputLines) Close() error {
	f.closed = true
	f.closeSeen = true
	return nil
}

func fakeDigitalOpenFn(lines *fakeDigitalInputLines, offsets *[]int) func(string, []int, string) (DigitalInputLines, error) {
	return func(chip string, off []int, consumer string) (DigitalInputLines, error) {
		*offsets = append([]int{}, off...)
		return lines, nil
	}
}

// loggingDigitalInputLines records "open"/"close" events (with the
// offsets an open call was for) into a shared log, so a test can assert
// the *order* open/close happen in - not just that both eventually
// occur - which is exactly what the real GPIO_V2_GET_LINE_IOCTL ordering
// bug (busy line still held by a not-yet-closed prior request) needs.
type loggingDigitalInputLines struct {
	log *[]string
}

func (f *loggingDigitalInputLines) GetValues() (uint64, error) { return 0, nil }
func (f *loggingDigitalInputLines) Close() error {
	*f.log = append(*f.log, "close")
	return nil
}

func loggingDigitalOpenFn(log *[]string) func(string, []int, string) (DigitalInputLines, error) {
	return func(chip string, off []int, consumer string) (DigitalInputLines, error) {
		cp := append([]int{}, off...)
		*log = append(*log, fmt.Sprintf("open %v", cp))
		return &loggingDigitalInputLines{log: log}, nil
	}
}

func TestDigitalInputModuleOpenLinesUsesEnabledChannelsInOrder(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DigitalInputModule.LiveEnabled = true
	cfg.DigitalInputModule.Channels[0] = DigitalInputChannelConfig{Enabled: true, Assigned: HydraulicOilLevel, Port: 7}
	cfg.DigitalInputModule.Channels[1] = DigitalInputChannelConfig{Enabled: true, Assigned: Motor1Powered, Port: 3}
	cfg.DigitalInputModule.Channels[2] = DigitalInputChannelConfig{Enabled: false}
	cfg.DigitalInputModule.Channels[3] = DigitalInputChannelConfig{Enabled: false}
	cfg.DigitalInputModule.Channels[4] = DigitalInputChannelConfig{Enabled: false}
	cfg.DigitalInputModule.Channels[5] = DigitalInputChannelConfig{Enabled: false}
	live := NewLiveConfig(cfg)

	fake := &fakeDigitalInputLines{}
	var openedOffsets []int
	dim := NewDigitalInputModule(live, fakeDigitalOpenFn(fake, &openedOffsets))

	if err := dim.openLines(cfg.DigitalInputModule); err != nil {
		t.Fatal(err)
	}
	if len(openedOffsets) != 2 || openedOffsets[0] != 7 || openedOffsets[1] != 3 {
		t.Fatalf("openedOffsets = %v, want [7 3]", openedOffsets)
	}

	// HydraulicOilLevel -> index 0 (bit 0), Motor1Powered -> index 1 (bit 1).
	fake.bits = 0x1
	v, ok := dim.ReadDigitalInput(HydraulicOilLevel)
	if !ok || !v {
		t.Fatalf("ReadDigitalInput(HydraulicOilLevel) = %v, ok=%v; want true, true", v, ok)
	}
	v, ok = dim.ReadDigitalInput(Motor1Powered)
	if !ok || v {
		t.Fatalf("ReadDigitalInput(Motor1Powered) = %v, ok=%v; want false, true", v, ok)
	}

	// A signal not assigned to any enabled channel must report not-ok,
	// so CompositeIO falls back to the base simulated IO.
	if _, ok := dim.ReadDigitalInput(Motor2Powered); ok {
		t.Error("Motor2Powered should not be reported as assigned (its channel is disabled)")
	}
}

func TestDigitalInputModuleNotOkWhenNotLive(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DigitalInputModule.LiveEnabled = false
	live := NewLiveConfig(cfg)

	dim := NewDigitalInputModule(live, nil)
	// Never opened - Run() wouldn't call openLines while LiveEnabled=false.

	// Assigned by default config, but no sim value has been set yet.
	if _, ok := dim.ReadDigitalInput(HydraulicOilLevel); ok {
		t.Error("expected ok=false before any sim value has been set")
	}
}

func TestDigitalInputModulePropagatesHardwareReadError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DigitalInputModule.LiveEnabled = true
	cfg.DigitalInputModule.Channels[0] = DigitalInputChannelConfig{Enabled: true, Assigned: HydraulicOilLevel, Port: 2}
	live := NewLiveConfig(cfg)

	fake := &fakeDigitalInputLines{getErr: errors.New("boom")}
	var offsets []int
	dim := NewDigitalInputModule(live, fakeDigitalOpenFn(fake, &offsets))
	if err := dim.openLines(cfg.DigitalInputModule); err != nil {
		t.Fatal(err)
	}

	if _, ok := dim.ReadDigitalInput(HydraulicOilLevel); ok {
		t.Error("expected ok=false on a hardware read error")
	}
}

func TestDigitalInputModuleReopensOnConfigChange(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DigitalInputModule.LiveEnabled = true
	cfg.DigitalInputModule.Channels = [DigitalInputChannelCount]DigitalInputChannelConfig{
		{Enabled: true, Assigned: HydraulicOilLevel, Port: 2},
	}
	live := NewLiveConfig(cfg)

	first := &fakeDigitalInputLines{}
	var firstOffsets []int
	dim := NewDigitalInputModule(live, fakeDigitalOpenFn(first, &firstOffsets))
	if err := dim.openLines(cfg.DigitalInputModule); err != nil {
		t.Fatal(err)
	}
	sigBefore := dim.currentSignature()

	// Re-point HydraulicOilLevel at a different line - the signature must
	// change so Run() knows to reopen rather than keep reading the old line.
	cfg2 := cfg
	cfg2.DigitalInputModule.Channels[0].Port = 13
	if digitalInputConfigSignature(cfg2.DigitalInputModule) == sigBefore {
		t.Fatal("signature did not change after re-mapping a line")
	}

	second := &fakeDigitalInputLines{}
	var secondOffsets []int
	dim.openFn = fakeDigitalOpenFn(second, &secondOffsets)
	if err := dim.openLines(cfg2.DigitalInputModule); err != nil {
		t.Fatal(err)
	}
	if !first.closeSeen {
		t.Error("expected the old line request to be closed on reopen")
	}
	if len(secondOffsets) != 1 || secondOffsets[0] != 13 {
		t.Errorf("secondOffsets = %v, want [13]", secondOffsets)
	}
}

// TestDigitalInputModuleClosesBeforeReopeningOverlappingLines reproduces
// a real GPIO_V2_GET_LINE_IOCTL failure seen commissioning against real
// hardware: enabling a second channel while a first is already live
// requests a new combined line set that *overlaps* the still-open old
// one (every previously-enabled line stays enabled), and the kernel
// refuses a second exclusive request for a line the same process is
// still holding open via the old handle. openLines must therefore close
// the old handle before requesting the new one, not after.
func TestDigitalInputModuleClosesBeforeReopeningOverlappingLines(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DigitalInputModule.LiveEnabled = true
	cfg.DigitalInputModule.Channels[0] = DigitalInputChannelConfig{Enabled: true, Assigned: HydraulicOilLevel, Port: 2}
	cfg.DigitalInputModule.Channels[1] = DigitalInputChannelConfig{Enabled: false}
	cfg.DigitalInputModule.Channels[2] = DigitalInputChannelConfig{Enabled: false}
	cfg.DigitalInputModule.Channels[3] = DigitalInputChannelConfig{Enabled: false}
	cfg.DigitalInputModule.Channels[4] = DigitalInputChannelConfig{Enabled: false}
	cfg.DigitalInputModule.Channels[5] = DigitalInputChannelConfig{Enabled: false}
	live := NewLiveConfig(cfg)

	var log []string
	dim := NewDigitalInputModule(live, loggingDigitalOpenFn(&log))
	if err := dim.openLines(cfg.DigitalInputModule); err != nil {
		t.Fatal(err)
	}

	// Enable a second channel - port 2 stays enabled, so the new
	// combined set [2 3] overlaps the still-open [2].
	cfg2 := cfg
	cfg2.DigitalInputModule.Channels[1] = DigitalInputChannelConfig{Enabled: true, Assigned: Motor1Powered, Port: 3}
	if err := dim.openLines(cfg2.DigitalInputModule); err != nil {
		t.Fatal(err)
	}

	want := []string{"open [2]", "close", "open [2 3]"}
	if len(log) != len(want) {
		t.Fatalf("log = %v, want %v", log, want)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("log = %v, want %v", log, want)
		}
	}
}

func TestDigitalInputSetSimValueRejectsUnassignedSignal(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DigitalInputModule.LiveEnabled = false
	cfg.DigitalInputModule.Channels[2] = DigitalInputChannelConfig{Enabled: false}
	cfg.DigitalInputModule.Channels[3] = DigitalInputChannelConfig{Enabled: false}
	cfg.DigitalInputModule.Channels[4] = DigitalInputChannelConfig{Enabled: false}
	cfg.DigitalInputModule.Channels[5] = DigitalInputChannelConfig{Enabled: false}
	live := NewLiveConfig(cfg)
	dim := NewDigitalInputModule(live, nil)

	if err := dim.SetSimValue(Motor2Powered, true); err == nil {
		t.Error("expected an error setting a sim value for a signal this module doesn't carry")
	}
}

func TestDigitalInputSetSimValueRejectsWhileLive(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DigitalInputModule.LiveEnabled = true
	live := NewLiveConfig(cfg)
	dim := NewDigitalInputModule(live, nil)

	if err := dim.SetSimValue(HydraulicOilLevel, true); err == nil {
		t.Error("expected an error setting a simulated value while live is on")
	}
}

func TestDigitalInputSetSimValueThenRead(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DigitalInputModule.LiveEnabled = false
	live := NewLiveConfig(cfg)
	dim := NewDigitalInputModule(live, nil)

	if err := dim.SetSimValue(HydraulicOilLevel, true); err != nil {
		t.Fatal(err)
	}
	v, ok := dim.ReadDigitalInput(HydraulicOilLevel)
	if !ok || !v {
		t.Fatalf("ReadDigitalInput(HydraulicOilLevel) = %v, ok=%v; want true, true", v, ok)
	}
}
