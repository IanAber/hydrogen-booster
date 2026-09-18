package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeW1Device(t *testing.T, root, deviceID, w1SlaveContent string) {
	t.Helper()
	dir := filepath.Join(root, deviceID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "w1_slave"), []byte(w1SlaveContent), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestReadOneWireTempCParsesGoodReading(t *testing.T) {
	dir := t.TempDir()
	old := w1DevicesPath
	w1DevicesPath = dir
	defer func() { w1DevicesPath = old }()

	writeW1Device(t, dir, "28-0000123456ef",
		"5a 01 4b 46 7f ff 0c 10 74 : crc=74 YES\n5a 01 4b 46 7f ff 0c 10 74 t=21625\n")

	v, err := readOneWireTempC("28-0000123456ef")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 21.625 {
		t.Errorf("got %v, want 21.625", v)
	}
}

func TestReadOneWireTempCRejectsBadCRC(t *testing.T) {
	dir := t.TempDir()
	old := w1DevicesPath
	w1DevicesPath = dir
	defer func() { w1DevicesPath = old }()

	// A garbled read: the driver's own CRC check failed (NO), which
	// happens on bus noise or marginal wiring - the value on line 2 must
	// not be trusted even though it's present.
	writeW1Device(t, dir, "28-0000123456ef",
		"00 00 00 00 00 00 00 00 00 : crc=00 NO\n00 00 00 00 00 00 00 00 00 t=12345\n")

	if _, err := readOneWireTempC("28-0000123456ef"); err == nil {
		t.Error("expected an error for a failed CRC check")
	}
}

func TestReadOneWireTempCMissingDevice(t *testing.T) {
	dir := t.TempDir()
	old := w1DevicesPath
	w1DevicesPath = dir
	defer func() { w1DevicesPath = old }()

	if _, err := readOneWireTempC("28-doesnotexist"); err == nil {
		t.Error("expected an error for a device with no w1_slave file")
	}
}

func TestDiscoverOneWireDevicesFiltersFamilyCode(t *testing.T) {
	dir := t.TempDir()
	old := w1DevicesPath
	w1DevicesPath = dir
	defer func() { w1DevicesPath = old }()

	writeW1Device(t, dir, "28-0000123456ef", "irrelevant")
	writeW1Device(t, dir, "28-0000abcdef01", "irrelevant")
	// Not a DS18B20 (different family code) and the bus master pseudo-device -
	// neither should show up as a selectable sensor.
	writeW1Device(t, dir, "10-000012345678", "irrelevant")
	writeW1Device(t, dir, "w1_bus_master1", "irrelevant")

	got := DiscoverOneWireDevices()
	want := []string{"28-0000123456ef", "28-0000abcdef01"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
			break
		}
	}
}

func TestDiscoverOneWireDevicesMissingBus(t *testing.T) {
	old := w1DevicesPath
	w1DevicesPath = filepath.Join(t.TempDir(), "does-not-exist")
	defer func() { w1DevicesPath = old }()

	if got := DiscoverOneWireDevices(); got != nil {
		t.Errorf("expected nil for a missing w1 bus, got %v", got)
	}
}

func fakeTempReader(values map[string]float64, errs map[string]error) func(string) (float64, error) {
	return func(deviceID string) (float64, error) {
		if err, ok := errs[deviceID]; ok {
			return 0, err
		}
		return values[deviceID], nil
	}
}

func TestTempPollOnceReadsAssignedChannels(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TempModule.LiveEnabled = true
	cfg.TempModule.Channels[0].DeviceID = "28-aaa" // Hyd1Temp by default assignment
	cfg.TempModule.Channels[1].DeviceID = "28-bbb" // Hyd2Temp
	live := NewLiveConfig(cfg)

	tm := NewTemperatureModule(live, fakeTempReader(map[string]float64{
		"28-aaa": 41.5,
		"28-bbb": 43.0,
	}, nil))
	tm.pollOnce(cfg.TempModule)

	v, ok := tm.ReadTempC(Hyd1Temp)
	if !ok || v != 41.5 {
		t.Errorf("Hyd1Temp = %v, ok=%v; want 41.5, true", v, ok)
	}
	v2, ok2 := tm.ReadTempC(Hyd2Temp)
	if !ok2 || v2 != 43.0 {
		t.Errorf("Hyd2Temp = %v, ok=%v; want 43.0, true", v2, ok2)
	}

	// A sensor not on this module at all must report not-ok, so
	// CompositeIO knows to fall back to the base simulated IO.
	if _, ok := tm.ReadTempC(TankTemp); ok {
		t.Error("TankTemp should not be reported as assigned (no DeviceID set)")
	}
}

func TestTempPollOnceRecordsPerChannelError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TempModule.LiveEnabled = true
	cfg.TempModule.Channels[0].DeviceID = "28-aaa"
	live := NewLiveConfig(cfg)

	tm := NewTemperatureModule(live, fakeTempReader(nil, map[string]error{
		"28-aaa": os.ErrNotExist,
	}))
	tm.pollOnce(cfg.TempModule)

	st := tm.Status()
	var found bool
	for _, ch := range st.Channels {
		if ch.Assigned == "Hyd1Temp" {
			found = true
			if ch.Ok {
				t.Error("expected Ok=false after a read error")
			}
			if ch.Error == "" {
				t.Error("expected a non-empty error message")
			}
		}
	}
	if !found {
		t.Fatal("Hyd1Temp channel missing from status")
	}
}

func TestTempLiveToSimSnapshotThenAdjustable(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TempModule.LiveEnabled = true
	live := NewLiveConfig(cfg)
	tm := NewTemperatureModule(live, nil)

	// Simulate a prior live reading.
	tm.cLive[TankTemp] = 18.5

	// Flip to simulation mode - snapshotToSim happens once, exactly as
	// Run() does on the true->false transition.
	c := live.Snapshot()
	c.TempModule.LiveEnabled = false
	live.Set(c)
	tm.snapshotToSim()

	v, ok := tm.ReadTempC(TankTemp)
	if !ok || v != 18.5 {
		t.Fatalf("expected the snapshotted 18.5 in sim mode, got %v ok=%v", v, ok)
	}

	if err := tm.SetSimValue(TankTemp, 25); err != nil {
		t.Fatal(err)
	}
	v2, _ := tm.ReadTempC(TankTemp)
	if v2 != 25 {
		t.Fatalf("expected the adjusted 25 after SetSimValue, got %v", v2)
	}

	// Setting a sim value while live is a mistake the API should reject.
	c2 := live.Snapshot()
	c2.TempModule.LiveEnabled = true
	live.Set(c2)
	if err := tm.SetSimValue(TankTemp, 1); err == nil {
		t.Error("expected an error setting a simulated value while live is on")
	}
}

func TestTempSetSimValueRejectsUnassignedSensor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TempModule.LiveEnabled = false
	// Nothing has a DeviceID by default, so no channel is "wired up",
	// but Hyd1Pressure specifically can never be a temperature channel.
	live := NewLiveConfig(cfg)
	tm := NewTemperatureModule(live, nil)

	if err := tm.SetSimValue(Hyd1Pressure, 20); err == nil {
		t.Error("expected an error setting a sim value for a sensor this module never carries")
	}
}

// TestTempModuleSurvivesFileRoundTrip confirms the temperature module's
// channel assignment and device mapping survive being written out and
// read back in, the same guarantee canconfig_roundtrip_test.go checks
// for the CAN pressure module.
func TestTempModuleSurvivesFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"

	base := DefaultConfig()
	base.TempModule.LiveEnabled = true
	base.TempModule.Channels[0].DeviceID = "28-0000123456ef"

	if err := SaveConfigFile(path, base); err != nil {
		t.Fatal(err)
	}

	loaded, ok, err := LoadConfigFile(path, DefaultConfig())
	if err != nil || !ok {
		t.Fatalf("load failed: ok=%v err=%v", ok, err)
	}
	if !loaded.TempModule.LiveEnabled {
		t.Error("LiveEnabled did not round-trip as true")
	}
	if loaded.TempModule.Channels[0].DeviceID != "28-0000123456ef" {
		t.Errorf("Channels[0].DeviceID = %q, want 28-0000123456ef", loaded.TempModule.Channels[0].DeviceID)
	}
	if loaded.TempModule.Channels[0].Assigned != Hyd1Temp {
		t.Errorf("Channels[0].Assigned = %v, want Hyd1Temp", loaded.TempModule.Channels[0].Assigned)
	}
}
