package main

import "testing"

// TestCanModuleSurvivesFileRoundTrip confirms the CAN module's channel
// assignment and calibration - the whole point of persisting config to
// a file - actually survives being written out and read back in, and
// that the file itself uses human-readable sensor names, not opaque
// enum numbers.
func TestCanModuleSurvivesFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"

	base := DefaultConfig()
	base.CanModule.Interface = "can1"
	base.CanModule.LiveEnabled = true
	base.CanModule.Channels[0].CalHighBar = 999

	if err := SaveConfigFile(path, base); err != nil {
		t.Fatal(err)
	}

	loaded, ok, err := LoadConfigFile(path, DefaultConfig())
	if err != nil || !ok {
		t.Fatalf("load failed: ok=%v err=%v", ok, err)
	}
	if loaded.CanModule.Interface != "can1" {
		t.Errorf("Interface = %q, want can1", loaded.CanModule.Interface)
	}
	if !loaded.CanModule.LiveEnabled {
		t.Error("LiveEnabled did not round-trip as true")
	}
	if loaded.CanModule.Channels[0].CalHighBar != 999 {
		t.Errorf("Channels[0].CalHighBar = %v, want 999", loaded.CanModule.Channels[0].CalHighBar)
	}
	if loaded.CanModule.Channels[0].Assigned != Hyd1Pressure {
		t.Errorf("Channels[0].Assigned = %v, want Hyd1Pressure", loaded.CanModule.Channels[0].Assigned)
	}
}

// TestRelayModuleSurvivesFileRoundTrip mirrors the same guarantee for the
// MCP23017 relay module's port assignment.
func TestRelayModuleSurvivesFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"

	base := DefaultConfig()
	base.RelayModule.Chip = "/dev/gpiochip3"
	base.RelayModule.LiveEnabled = true
	base.RelayModule.Channels[0].Port = 12

	if err := SaveConfigFile(path, base); err != nil {
		t.Fatal(err)
	}

	loaded, ok, err := LoadConfigFile(path, DefaultConfig())
	if err != nil || !ok {
		t.Fatalf("load failed: ok=%v err=%v", ok, err)
	}
	if loaded.RelayModule.Chip != "/dev/gpiochip3" {
		t.Errorf("Chip = %q, want /dev/gpiochip3", loaded.RelayModule.Chip)
	}
	if !loaded.RelayModule.LiveEnabled {
		t.Error("LiveEnabled did not round-trip as true")
	}
	if loaded.RelayModule.Channels[0].Port != 12 {
		t.Errorf("Channels[0].Port = %v, want 12", loaded.RelayModule.Channels[0].Port)
	}
	if loaded.RelayModule.Channels[0].Assigned != Valve1A {
		t.Errorf("Channels[0].Assigned = %v, want Valve1A", loaded.RelayModule.Channels[0].Assigned)
	}
}
