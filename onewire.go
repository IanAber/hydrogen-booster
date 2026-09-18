package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// w1DevicesPath is a var, not a const, so tests can point it at a
// temporary directory instead of the real sysfs tree.
var w1DevicesPath = "/sys/bus/w1/devices"

// DiscoverOneWireDevices lists DS18B20 (1-Wire family code 28) sensors
// currently visible on the system's bus, e.g. "28-0000123456ef", for the
// admin screen's device picker. Returns an empty slice (not an error) if
// the w1 subsystem isn't present - off-target development, or the
// w1-gpio/w1-therm kernel modules aren't loaded yet - rather than failing
// the whole config load over it.
func DiscoverOneWireDevices() []string {
	entries, err := os.ReadDir(w1DevicesPath)
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "28-") {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids
}

// readOneWireTempC reads one DS18B20 via the kernel w1-therm driver's
// w1_slave sysfs file, which looks like:
//
//	5a 01 4b 46 7f ff 0c 10 74 : crc=74 YES
//	5a 01 4b 46 7f ff 0c 10 74 t=21625
//
// The trailing YES/NO on line 1 is the driver's own scratchpad CRC check:
// NO means a garbled read (bus noise, marginal wiring) - discard it
// rather than report a bogus temperature. Line 2's t= value is
// millidegrees C.
func readOneWireTempC(deviceID string) (float64, error) {
	data, err := os.ReadFile(filepath.Join(w1DevicesPath, deviceID, "w1_slave"))
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", deviceID, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 || !strings.HasSuffix(strings.TrimSpace(lines[0]), "YES") {
		return 0, fmt.Errorf("%s: bad CRC / no reading", deviceID)
	}
	idx := strings.Index(lines[1], "t=")
	if idx < 0 {
		return 0, fmt.Errorf("%s: unexpected w1_slave format", deviceID)
	}
	milliC, err := strconv.Atoi(strings.TrimSpace(lines[1][idx+2:]))
	if err != nil {
		return 0, fmt.Errorf("%s: parse temperature: %w", deviceID, err)
	}
	return float64(milliC) / 1000.0, nil
}

// TemperatureModule polls DS18B20 1-Wire sensors on a timer, publishing a
// °C value per assigned SensorInput for CompositeIO to serve to the rest
// of the system. Mirrors PressureModule's live/simulation switch: while
// TempModule.LiveEnabled is on, published values come from readFn (the
// real 1-Wire bus in production); the moment it's switched off, the last
// live reading for every assigned channel is copied once into an
// editable simulated value, after which SetSimValue can adjust those
// values in real time - e.g. from admin.html - without touching any
// hardware at all.
type TemperatureModule struct {
	live   *LiveConfig
	readFn func(deviceID string) (float64, error) // swappable for tests

	mu         sync.RWMutex
	cLive      map[SensorInput]float64
	cSim       map[SensorInput]float64
	lastErr    map[SensorInput]string
	lastUpdate time.Time

	stop chan struct{}
}

// NewTemperatureModule constructs a module against live config. readFn
// reads one device's temperature - pass readOneWireTempC for the real
// driver; tests pass a fake instead.
func NewTemperatureModule(live *LiveConfig, readFn func(deviceID string) (float64, error)) *TemperatureModule {
	return &TemperatureModule{
		live:    live,
		readFn:  readFn,
		cLive:   map[SensorInput]float64{},
		cSim:    map[SensorInput]float64{},
		lastErr: map[SensorInput]string{},
		stop:    make(chan struct{}),
	}
}

// Run polls for the life of the program; call it once in a goroutine.
func (m *TemperatureModule) Run() {
	wasLive := false
	for {
		select {
		case <-m.stop:
			return
		default:
		}

		cfg := m.live.Snapshot().TempModule

		if !cfg.LiveEnabled {
			if wasLive {
				m.snapshotToSim()
				log.Printf("Temperature module: live data OFF - current readings loaded into simulator")
			}
			wasLive = false
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if !wasLive {
			log.Printf("Temperature module: live data ON")
		}
		wasLive = true

		m.pollOnce(cfg)
		time.Sleep(cfg.PollPeriod)
	}
}

func (m *TemperatureModule) Stop() {
	close(m.stop)
}

func (m *TemperatureModule) pollOnce(cfg TempModuleConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range cfg.Channels {
		if !ch.Enabled || ch.DeviceID == "" {
			continue
		}
		v, err := m.readFn(ch.DeviceID)
		if err != nil {
			m.lastErr[ch.Assigned] = err.Error()
			continue
		}
		m.cLive[ch.Assigned] = v
		delete(m.lastErr, ch.Assigned)
	}
	m.lastUpdate = time.Now()
}

// snapshotToSim copies the last known live value for every assigned
// channel into the simulated set - called once, at the moment live data
// is switched off.
func (m *TemperatureModule) snapshotToSim() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.cLive {
		m.cSim[k] = v
	}
}

func (m *TemperatureModule) assignedChannel(cfg TempModuleConfig, sensor SensorInput) (int, bool) {
	for i, ch := range cfg.Channels {
		if ch.Enabled && ch.Assigned == sensor {
			return i, true
		}
	}
	return 0, false
}

// ReadTempC returns the current value for sensor if (and only if) it's
// assigned to an enabled channel on this module; ok is false otherwise,
// so CompositeIO can fall back to the base (still-simulated) IO for
// anything not wired to this module.
func (m *TemperatureModule) ReadTempC(sensor SensorInput) (value float64, ok bool) {
	cfg := m.live.Snapshot().TempModule
	if _, assigned := m.assignedChannel(cfg, sensor); !assigned {
		return 0, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if cfg.LiveEnabled {
		v, ok := m.cLive[sensor]
		return v, ok
	}
	v, ok := m.cSim[sensor]
	return v, ok
}

// SetSimValue adjusts one channel's simulated value in real time (e.g.
// from an admin.html input). Returns an error if that sensor isn't
// assigned to an enabled channel, or if the module is currently live
// (simulated values only apply while live data is off).
func (m *TemperatureModule) SetSimValue(sensor SensorInput, c float64) error {
	cfg := m.live.Snapshot().TempModule
	if cfg.LiveEnabled {
		return fmt.Errorf("cannot set a simulated value while live data is on")
	}
	if _, assigned := m.assignedChannel(cfg, sensor); !assigned {
		return fmt.Errorf("%s is not assigned to an enabled channel", sensor)
	}
	m.mu.Lock()
	m.cSim[sensor] = c
	m.mu.Unlock()
	return nil
}

type TempChannelStatus struct {
	Index    int     `json:"index"`
	Enabled  bool    `json:"enabled"`
	Assigned string  `json:"assigned,omitempty"`
	DeviceID string  `json:"deviceId,omitempty"`
	TempC    float64 `json:"tempC"`
	Ok       bool    `json:"ok"`
	Error    string  `json:"error,omitempty"`
}

type TemperatureModuleStatus struct {
	LiveEnabled      bool                `json:"liveEnabled"`
	LastUpdate       *time.Time          `json:"lastUpdate,omitempty"`
	Channels         []TempChannelStatus `json:"channels"`
	AvailableDevices []string            `json:"availableDevices"`
}

func (m *TemperatureModule) Status() TemperatureModuleStatus {
	cfg := m.live.Snapshot().TempModule
	devices := DiscoverOneWireDevices()

	m.mu.RLock()
	defer m.mu.RUnlock()

	st := TemperatureModuleStatus{LiveEnabled: cfg.LiveEnabled, AvailableDevices: devices}
	if !m.lastUpdate.IsZero() {
		t := m.lastUpdate
		st.LastUpdate = &t
	}
	for i, ch := range cfg.Channels {
		cs := TempChannelStatus{Index: i, Enabled: ch.Enabled, DeviceID: ch.DeviceID}
		if ch.Enabled {
			cs.Assigned = ch.Assigned.String()
			if cfg.LiveEnabled {
				cs.TempC, cs.Ok = m.cLive[ch.Assigned]
				if errMsg, has := m.lastErr[ch.Assigned]; has {
					cs.Error = errMsg
					cs.Ok = false
				}
			} else {
				cs.TempC, cs.Ok = m.cSim[ch.Assigned]
			}
		}
		st.Channels = append(st.Channels, cs)
	}
	return st
}
