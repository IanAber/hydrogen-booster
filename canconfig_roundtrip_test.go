package main

import (
	"encoding/json"
	"os"
	"testing"
)

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

// TestDigitalInputModuleSurvivesFileRoundTrip mirrors the same guarantee
// for the digital input module's line assignment.
func TestDigitalInputModuleSurvivesFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"

	base := DefaultConfig()
	base.DigitalInputModule.Chip = "/dev/gpiochip3"
	base.DigitalInputModule.LiveEnabled = true
	base.DigitalInputModule.Channels[0].Port = 16

	if err := SaveConfigFile(path, base); err != nil {
		t.Fatal(err)
	}

	loaded, ok, err := LoadConfigFile(path, DefaultConfig())
	if err != nil || !ok {
		t.Fatalf("load failed: ok=%v err=%v", ok, err)
	}
	if loaded.DigitalInputModule.Chip != "/dev/gpiochip3" {
		t.Errorf("Chip = %q, want /dev/gpiochip3", loaded.DigitalInputModule.Chip)
	}
	if !loaded.DigitalInputModule.LiveEnabled {
		t.Error("LiveEnabled did not round-trip as true")
	}
	if loaded.DigitalInputModule.Channels[0].Port != 16 {
		t.Errorf("Channels[0].Port = %v, want 16", loaded.DigitalInputModule.Channels[0].Port)
	}
	if loaded.DigitalInputModule.Channels[0].Assigned != HydraulicOilLevel {
		t.Errorf("Channels[0].Assigned = %v, want HydraulicOilLevel", loaded.DigitalInputModule.Channels[0].Assigned)
	}
}

// TestDigitalInputModuleLoadsOlderConfigWithFewerChannels checks that a
// config file saved before HydrogenDetected existed (3 digital input
// channels) still loads, with the missing slot left disabled, instead
// of failing validation and dropping every saved setting.
func TestDigitalInputModuleLoadsOlderConfigWithFewerChannels(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"

	base := DefaultConfig()
	base.CoolingFan.SetpointC = 61
	dto := ToDTO(base)
	dto.DigitalInputModule.Channels = dto.DigitalInputModule.Channels[:3]
	data, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	loaded, ok, err := LoadConfigFile(path, DefaultConfig())
	if err != nil || !ok {
		t.Fatalf("load failed: ok=%v err=%v", ok, err)
	}
	if loaded.CoolingFan.SetpointC != 61 {
		t.Errorf("CoolingFan.SetpointC = %v, want 61", loaded.CoolingFan.SetpointC)
	}
	if loaded.DigitalInputModule.Channels[2].Assigned != Motor2Powered {
		t.Errorf("Channels[2].Assigned = %v, want Motor2Powered", loaded.DigitalInputModule.Channels[2].Assigned)
	}
	if loaded.DigitalInputModule.Channels[3].Enabled {
		t.Error("Channels[3] should be disabled when missing from the file")
	}
}
