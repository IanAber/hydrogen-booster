package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPressureModuleLiveNeverReadIsPendingThenError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CanModule.LiveEnabled = true
	pm := NewPressureModule(NewLiveConfig(cfg), nil)

	pm.liveSince = time.Now()
	if _, handled, err := pm.readForIO(Hyd1Pressure); !handled || !errors.Is(err, ErrSensorPending) {
		t.Fatalf("handled=%v err=%v, want pending within the startup grace period", handled, err)
	}

	pm.liveSince = time.Now().Add(-time.Minute)
	pm.lastErr = "recv: timeout"
	_, handled, err := pm.readForIO(Hyd1Pressure)
	if !handled || err == nil || errors.Is(err, ErrSensorPending) || !strings.Contains(err.Error(), "recv: timeout") {
		t.Fatalf("handled=%v err=%v, want a real error including the last comms error", handled, err)
	}
}

func TestPressureModuleLiveStaleReadingIsError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CanModule.LiveEnabled = true
	pm := NewPressureModule(NewLiveConfig(cfg), nil)
	pm.liveSince = time.Now().Add(-time.Minute)
	pm.barLive[Hyd1Pressure] = 100

	pm.lastUpdate = time.Now()
	if v, _, err := pm.readForIO(Hyd1Pressure); err != nil || v != 100 {
		t.Fatalf("fresh reading: v=%v err=%v", v, err)
	}

	pm.lastUpdate = time.Now().Add(-30 * time.Second)
	if _, handled, err := pm.readForIO(Hyd1Pressure); !handled || err == nil {
		t.Fatalf("stale reading: handled=%v err=%v, want an error", handled, err)
	}
}

func TestPressureModuleLiveUnassignedSensorIsErrorNotSimulated(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CanModule.LiveEnabled = true
	cfg.CanModule.Channels[2] = CanChannelConfig{Enabled: false} // TankPressure
	pm := NewPressureModule(NewLiveConfig(cfg), nil)
	if _, handled, err := pm.readForIO(TankPressure); !handled || err == nil {
		t.Fatalf("handled=%v err=%v, want an error (not a fallback to simulation)", handled, err)
	}
}

func TestPressureModuleSimulationFallsBackAsBefore(t *testing.T) {
	pm := NewPressureModule(NewLiveConfig(DefaultConfig()), nil)
	if _, handled, err := pm.readForIO(Hyd1Pressure); handled || err != nil {
		t.Fatalf("handled=%v err=%v, want unhandled (no sim value set) so CompositeIO uses the simulator", handled, err)
	}
}

func TestTemperatureModuleLiveWithoutDeviceIsError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TempModule.LiveEnabled = true // default channels have no DeviceID selected
	tm := NewTemperatureModule(NewLiveConfig(cfg), nil)
	_, handled, err := tm.readForIO(HydOilTemp)
	if !handled || err == nil || !strings.Contains(err.Error(), "no 1-Wire device") {
		t.Fatalf("handled=%v err=%v, want a 'no device selected' error", handled, err)
	}
}

func TestTemperatureModuleLiveStaleReadingIsError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TempModule.LiveEnabled = true
	cfg.TempModule.Channels[0].DeviceID = "28-000000000001"
	tm := NewTemperatureModule(NewLiveConfig(cfg), nil)
	tm.liveSince = time.Now().Add(-time.Hour)
	tm.cLive[HydOilTemp] = 40
	tm.lastGood[HydOilTemp] = time.Now().Add(-time.Minute) // > 3x the 5s poll period
	tm.lastErr[HydOilTemp] = "CRC check failed"

	_, _, err := tm.readForIO(HydOilTemp)
	if err == nil || !strings.Contains(err.Error(), "CRC check failed") {
		t.Fatalf("err=%v, want a stale-reading error including the last read error", err)
	}
}

func TestDigitalInputModuleLiveLinesNotOpenIsError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DigitalInputModule.LiveEnabled = true
	dim := NewDigitalInputModule(NewLiveConfig(cfg), nil)

	dim.liveSince = time.Now()
	if _, _, err := dim.readForIO(HydrogenDetected); !errors.Is(err, ErrSensorPending) {
		t.Fatalf("err=%v, want pending while the lines are first opening", err)
	}

	dim.liveSince = time.Now().Add(-time.Minute)
	dim.lastOpenErr = "open /dev/gpiochip0: permission denied"
	_, handled, err := dim.readForIO(HydrogenDetected)
	if !handled || err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("handled=%v err=%v, want an error including the open failure", handled, err)
	}
}

// sensorFaultIO lets a test make individual sensors unreadable.
type sensorFaultIO struct {
	*SimulatedIO
	badSensor  map[SensorInput]error
	badDigital map[DigitalInput]error
}

func (f *sensorFaultIO) ReadPressureBar(s SensorInput) (float64, error) {
	if err := f.badSensor[s]; err != nil {
		return 0, err
	}
	return f.SimulatedIO.ReadPressureBar(s)
}

func (f *sensorFaultIO) ReadTempC(s SensorInput) (float64, error) {
	if err := f.badSensor[s]; err != nil {
		return 0, err
	}
	return f.SimulatedIO.ReadTempC(s)
}

func (f *sensorFaultIO) ReadDigitalInput(d DigitalInput) (bool, error) {
	if err := f.badDigital[d]; err != nil {
		return false, err
	}
	return f.SimulatedIO.ReadDigitalInput(d)
}

func newSensorFaultController(t *testing.T, cfg Config) (*Controller, *sensorFaultIO) {
	sim := NewSimulatedIO()
	t.Cleanup(func() { sim.Close() })
	io := &sensorFaultIO{SimulatedIO: sim, badSensor: map[SensorInput]error{}, badDigital: map[DigitalInput]error{}}
	return NewController(NewLiveConfig(cfg), io), io
}

func TestSuperviseSafetyFaultsOnUnreadableSensorNamingIt(t *testing.T) {
	cases := []struct {
		name string
		set  func(*sensorFaultIO)
		want string
	}{
		{"pressure", func(io *sensorFaultIO) { io.badSensor[InterStagePressure] = errors.New("recv: timeout") }, "Inter-stage pressure sensor unreadable"},
		{"temperature", func(io *sensorFaultIO) { io.badSensor[OutputTemp] = errors.New("CRC check failed") }, "Hydrogen output temperature sensor unreadable"},
		{"hydrogen", func(io *sensorFaultIO) { io.badDigital[HydrogenDetected] = errors.New("GPIO read failed") }, "Hydrogen detector sensor unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, io := newSensorFaultController(t, DefaultConfig())
			_ = io.SetRelay(Pump1, true)
			tc.set(io)
			c.superviseSafety()

			st := c.Status()
			if st.Phase != PhaseFault {
				t.Fatalf("phase = %v, want fault", st.Phase)
			}
			if len(st.Faults) == 0 || !strings.Contains(st.Faults[0], tc.want) {
				t.Fatalf("faults = %v, want one containing %q", st.Faults, tc.want)
			}
			if on, _ := io.RelayState(Pump1); on {
				t.Fatal("Pump1 should have been stopped")
			}
		})
	}
}

func TestSuperviseSafetyFaultsBelowMinimum(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Safety.Limits[HydOilTemp] = SensorLimit{Min: 50, Max: 85}
	c, io := newSensorFaultController(t, cfg)
	io.temps[HydOilTemp] = 41 // the simulator's normal oil temperature
	c.superviseSafety()

	st := c.Status()
	if st.Phase != PhaseFault || len(st.Faults) == 0 || !strings.Contains(st.Faults[0], "Hydraulic oil temperature") || !strings.Contains(st.Faults[0], "below its minimum") {
		t.Fatalf("phase=%v faults=%v, want a below-minimum fault naming the oil temperature", st.Phase, st.Faults)
	}
}

func TestSuperviseSafetyDoesNotFaultWhileSensorPending(t *testing.T) {
	c, io := newSensorFaultController(t, DefaultConfig())
	io.badSensor[Hyd1Pressure] = ErrSensorPending
	c.superviseSafety()
	if got := c.Status().Phase; got == PhaseFault {
		t.Fatal("a sensor still waiting for its first reading should not fault")
	}
	err := c.Start(JobRequest{Nozzle: Nozzle350, TargetPressureBar: 100, TargetMassKg: 1})
	if err == nil || !strings.Contains(err.Error(), "Hydraulic 1 pressure") {
		t.Fatalf("Start err = %v, want a refusal naming the sensor", err)
	}
}

func TestSuperviseSafetyRecordsPersistingProblemOnce(t *testing.T) {
	c, io := newSensorFaultController(t, DefaultConfig())
	io.badSensor[TankPressure] = errors.New("recv: timeout")
	for i := 0; i < 20; i++ {
		c.superviseSafety()
	}
	io.badSensor[OutputTemp] = errors.New("CRC check failed")
	c.superviseSafety()

	if faults := c.Status().Faults; len(faults) != 2 {
		t.Fatalf("faults = %v, want exactly one entry per problem sensor", faults)
	}
}

func TestStartRefusedWhileSensorUnreadable(t *testing.T) {
	c, io := newSensorFaultController(t, DefaultConfig())
	io.badSensor[OutputPressure] = errors.New("recv: timeout")
	err := c.Start(JobRequest{Nozzle: Nozzle350, TargetPressureBar: 100, TargetMassKg: 1})
	if err == nil || !strings.Contains(err.Error(), "Hydrogen output pressure sensor unreadable") {
		t.Fatalf("Start err = %v, want a refusal naming the sensor", err)
	}
}

func TestManualPumpRefusedWhileFaulted(t *testing.T) {
	c, _ := newSensorFaultController(t, DefaultConfig())
	if err := c.SetManualMode(true); err != nil {
		t.Fatal(err)
	}
	c.raiseFault("test fault")
	if err := c.ManualSetPump(1, true); err == nil {
		t.Fatal("ManualSetPump(on) should be refused while a fault is active")
	}
	if err := c.ManualSetPump(1, false); err != nil {
		t.Fatalf("turning a pump off should always be allowed: %v", err)
	}
}

func TestSafetyLimitsMigrateFromOlderConfigFile(t *testing.T) {
	path := t.TempDir() + "/config.json"
	dto := ToDTO(DefaultConfig())
	hyd, h2, temp := 25.0, 700.0, 70.0
	dto.Safety = SafetyConfigDTO{SensorMaxAgeMs: 2000, MaxHydraulicPressureBar: &hyd, MaxHydrogenPressureBar: &h2, MaxTempC: &temp}
	data, _ := json.Marshal(dto)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	loaded, ok, err := LoadConfigFile(path, DefaultConfig())
	if err != nil || !ok {
		t.Fatalf("load failed: ok=%v err=%v", ok, err)
	}
	l := loaded.Safety.Limits
	if l[Hyd1Pressure].Max != 25 || l[Hyd2Pressure].Max != 25 || l[OutputPressure].Max != 700 || l[HydOilTemp].Max != 70 || l[OutputTemp].Max != 70 {
		t.Fatalf("limits not migrated: %+v", l)
	}
	if l[TankPressure] != DefaultConfig().Safety.Limits[TankPressure] {
		t.Errorf("TankPressure limit = %+v, want the default", l[TankPressure])
	}

	// Saving writes the new per-sensor format only.
	if err := SaveConfigFile(path, loaded); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(path)
	if strings.Contains(string(saved), "maxHydraulicPressureBar") || !strings.Contains(string(saved), "sensorLimits") {
		t.Fatalf("saved file should use sensorLimits only:\n%s", saved)
	}
}

func TestSafetyLimitsRejectMinNotBelowMax(t *testing.T) {
	dto := ToDTO(DefaultConfig())
	dto.Safety.SensorLimits[0].Min = 300
	dto.Safety.SensorLimits[0].Max = 250
	if _, err := FromDTO(DefaultConfig(), dto); err == nil {
		t.Fatal("expected min >= max to be rejected")
	}
}
