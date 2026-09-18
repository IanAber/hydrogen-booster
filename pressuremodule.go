package main

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// PressureModule drives the 6-channel 4-20mA CAN pressure input module:
// on a timer, it sends the "read all channels" request, parses the
// response frames, applies each channel's two-point linear calibration,
// and publishes a bar value per assigned SensorInput for CompositeIO to
// serve to the rest of the control system.
//
// It also implements the live/simulation switch described on the admin
// screen: while CanModule.LiveEnabled is on, published values come from
// the CAN bus and the socket is kept open; the moment it's switched off,
// the last live reading for every assigned channel is copied once into
// an editable simulated value (and the socket is released), after which
// SetSimValue can adjust those values in real time - e.g. from
// admin.html sliders - without touching the bus at all.
type PressureModule struct {
	live      *LiveConfig
	transport CanTransport // only touched by the Run goroutine; nil whenever not live
	openFn    func(ifaceName string) (CanTransport, error)

	mu         sync.RWMutex
	raw        [CanChannelCount]int
	barLive    map[SensorInput]float64
	barSim     map[SensorInput]float64
	commsOK    bool
	lastErr    string
	lastUpdate time.Time

	stop chan struct{}
}

// NewPressureModule constructs a module against live config. openFn
// opens the transport for a given interface name - pass OpenSocketCAN
// for the real driver; tests pass a fake instead.
func NewPressureModule(live *LiveConfig, openFn func(ifaceName string) (CanTransport, error)) *PressureModule {
	return &PressureModule{
		live:    live,
		openFn:  openFn,
		barLive: map[SensorInput]float64{},
		barSim:  map[SensorInput]float64{},
		stop:    make(chan struct{}),
	}
}

// Run polls for the life of the program; call it once in a goroutine.
// It only opens the CAN socket while LiveEnabled is on, and releases it
// the moment simulation mode is switched on, so a machine that never
// enables live data (e.g. during development, or before the CAN wiring
// is commissioned) never needs can0 to exist at all.
func (m *PressureModule) Run() {
	currentIface := ""
	wasLive := false

	for {
		select {
		case <-m.stop:
			return
		default:
		}

		cfg := m.live.Snapshot().CanModule

		if !cfg.LiveEnabled {
			if wasLive {
				m.snapshotToSim()
				log.Printf("Pressure module: live data OFF - current readings loaded into simulator")
			}
			wasLive = false
			if m.transport != nil {
				_ = m.transport.Close()
				m.transport = nil
				currentIface = ""
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if m.transport == nil || cfg.Interface != currentIface {
			t, err := m.openFn(cfg.Interface)
			if err != nil {
				m.mu.Lock()
				m.commsOK = false
				m.lastErr = fmt.Sprintf("open %s: %v", cfg.Interface, err)
				m.mu.Unlock()
				time.Sleep(1 * time.Second)
				continue
			}
			m.transport = t
			currentIface = cfg.Interface
			log.Printf("Pressure module: opened CAN interface %s", cfg.Interface)
		}

		if !wasLive {
			log.Printf("Pressure module: live data ON")
		}
		wasLive = true

		m.pollOnce(cfg)
		time.Sleep(cfg.PollPeriod)
	}
}

func (m *PressureModule) Stop() {
	close(m.stop)
	if m.transport != nil {
		_ = m.transport.Close()
	}
}

func (m *PressureModule) pollOnce(cfg CanModuleConfig) {
	if err := m.transport.Send(BuildReadAllRequest(cfg)); err != nil {
		m.recordError(fmt.Errorf("send: %w", err))
		return
	}

	var raw [CanChannelCount]int
	received := 0
	needed := (cfg.ChannelCount + 2) / 3
	deadline := time.Now().Add(cfg.ResponseTimeout)

	for received < needed {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			m.recordError(fmt.Errorf("timed out waiting for response (%d/%d frames)", received, needed))
			return
		}
		f, err := m.transport.Recv(remaining)
		if err != nil {
			m.recordError(fmt.Errorf("recv: %w", err))
			return
		}
		if f.ID != cfg.ResponseID {
			continue
		}
		pr, err := ParseResponseFrame(f)
		if err != nil || pr.FuncCode != cfg.FunctionCode {
			continue
		}
		for i, v := range pr.Values {
			idx := pr.Offset + i
			if idx >= 0 && idx < cfg.ChannelCount {
				raw[idx] = v
			}
		}
		received++
	}

	m.mu.Lock()
	m.raw = raw
	for i := 0; i < cfg.ChannelCount && i < len(cfg.Channels); i++ {
		ch := cfg.Channels[i]
		if !ch.Enabled {
			continue
		}
		m.barLive[ch.Assigned] = ch.ToBar(raw[i])
	}
	m.commsOK = true
	m.lastErr = ""
	m.lastUpdate = time.Now()
	m.mu.Unlock()
}

func (m *PressureModule) recordError(err error) {
	m.mu.Lock()
	m.commsOK = false
	m.lastErr = err.Error()
	m.mu.Unlock()
}

// snapshotToSim copies the last known live bar value for every assigned
// channel into the simulated set - called once, at the moment live data
// is switched off.
func (m *PressureModule) snapshotToSim() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.barLive {
		m.barSim[k] = v
	}
}

func (m *PressureModule) assignedChannel(cfg CanModuleConfig, sensor SensorInput) (int, bool) {
	for i := 0; i < cfg.ChannelCount && i < len(cfg.Channels); i++ {
		if cfg.Channels[i].Enabled && cfg.Channels[i].Assigned == sensor {
			return i, true
		}
	}
	return 0, false
}

// ReadPressureBar returns the current value for sensor if (and only if)
// it's assigned to an enabled channel on this module; ok is false
// otherwise, so CompositeIO can fall back to the base (still-simulated)
// IO for anything not wired to this module.
func (m *PressureModule) ReadPressureBar(sensor SensorInput) (value float64, ok bool) {
	cfg := m.live.Snapshot().CanModule
	if _, assigned := m.assignedChannel(cfg, sensor); !assigned {
		return 0, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if cfg.LiveEnabled {
		v, ok := m.barLive[sensor]
		return v, ok
	}
	v, ok := m.barSim[sensor]
	return v, ok
}

// SetSimValue adjusts one channel's simulated value in real time (e.g.
// from an admin.html slider). Returns an error if that sensor isn't
// assigned to an enabled channel, or if the module is currently live
// (simulated values only apply while live data is off).
func (m *PressureModule) SetSimValue(sensor SensorInput, bar float64) error {
	cfg := m.live.Snapshot().CanModule
	if cfg.LiveEnabled {
		return fmt.Errorf("cannot set a simulated value while live data is on")
	}
	if _, assigned := m.assignedChannel(cfg, sensor); !assigned {
		return fmt.Errorf("%s is not assigned to an enabled channel", sensor)
	}
	m.mu.Lock()
	m.barSim[sensor] = bar
	m.mu.Unlock()
	return nil
}

type PressureChannelStatus struct {
	Index    int     `json:"index"`
	Enabled  bool    `json:"enabled"`
	Assigned string  `json:"assigned,omitempty"`
	Raw      int     `json:"raw"`
	Bar      float64 `json:"bar"`
}

type PressureModuleStatus struct {
	Interface   string                  `json:"interface"`
	LiveEnabled bool                    `json:"liveEnabled"`
	CommsOK     bool                    `json:"commsOk"`
	LastError   string                  `json:"lastError,omitempty"`
	LastUpdate  *time.Time              `json:"lastUpdate,omitempty"`
	Channels    []PressureChannelStatus `json:"channels"`
}

func (m *PressureModule) Status() PressureModuleStatus {
	cfg := m.live.Snapshot().CanModule
	m.mu.RLock()
	defer m.mu.RUnlock()

	st := PressureModuleStatus{
		Interface: cfg.Interface, LiveEnabled: cfg.LiveEnabled,
		CommsOK: m.commsOK, LastError: m.lastErr,
	}
	if !m.lastUpdate.IsZero() {
		t := m.lastUpdate
		st.LastUpdate = &t
	}
	for i := 0; i < cfg.ChannelCount && i < len(cfg.Channels); i++ {
		ch := cfg.Channels[i]
		cs := PressureChannelStatus{Index: i, Enabled: ch.Enabled}
		if ch.Enabled {
			cs.Assigned = ch.Assigned.String()
			cs.Raw = m.raw[i]
			if cfg.LiveEnabled {
				cs.Bar = m.barLive[ch.Assigned]
			} else {
				cs.Bar = m.barSim[ch.Assigned]
			}
		}
		st.Channels = append(st.Channels, cs)
	}
	return st
}
