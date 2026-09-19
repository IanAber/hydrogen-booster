package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// DigitalInputLines abstracts one open line-request handle for reading
// digital feedback signals - a group of GPIO lines reserved together as
// inputs, addressed by their position within the request (0..n-1).
// OpenDigitalInputLines (gpioinput_linux.go) is the real implementation;
// tests pass a fake. Read-only: unlike GpioLines (relay outputs), inputs
// are never written.
type DigitalInputLines interface {
	// GetValues returns the current level of every line in this
	// request, bit i = line i.
	GetValues() (uint64, error)
	Close() error
}

// DigitalInputModule reads digital feedback signals (hydraulic oil
// level, motor-powered confirmation) through the SoC's own GPIO
// controller, addressed via the kernel gpiochip character device -
// gpiochip0, distinct from the MCP23017 relay expander RelayModule
// drives on gpiochip16. Mirrors RelayModule's live/simulation switch:
// while DigitalInputModule.LiveEnabled is on, ReadDigitalInput for an
// assigned, enabled channel reads the real GPIO line; otherwise (or for
// any signal not assigned to an enabled channel) it serves an
// admin-editable simulated value instead, so fault conditions can be
// exercised without real wiring. Starts disabled so nothing is read
// from real hardware until an operator has confirmed port wiring from
// the admin screen.
type DigitalInputModule struct {
	live   *LiveConfig
	openFn func(chipPath string, offsets []int, consumer string) (DigitalInputLines, error)

	mu        sync.Mutex
	lines     DigitalInputLines
	openSig   string // config signature the currently-open `lines` was opened for
	portIndex map[DigitalInput]int
	simValues map[DigitalInput]bool

	stop chan struct{}
}

// NewDigitalInputModule constructs a module against live config. openFn
// opens a group of GPIO lines as inputs - pass OpenDigitalInputLines for
// the real driver; tests pass a fake instead.
func NewDigitalInputModule(live *LiveConfig, openFn func(chipPath string, offsets []int, consumer string) (DigitalInputLines, error)) *DigitalInputModule {
	return &DigitalInputModule{live: live, openFn: openFn, simValues: map[DigitalInput]bool{}, stop: make(chan struct{})}
}

// digitalInputConfigSignature changes whenever the chip path or the set
// of enabled channel->port assignments changes, so Run() knows to
// re-request the GPIO lines rather than silently keep reading a stale
// mapping after the operator edits it.
func digitalInputConfigSignature(cfg DigitalInputModuleConfig) string {
	var b strings.Builder
	b.WriteString(cfg.Chip)
	for _, ch := range cfg.Channels {
		if ch.Enabled {
			fmt.Fprintf(&b, "|%s:%d", ch.Assigned.String(), ch.Port)
		}
	}
	return b.String()
}

// Run polls for the life of the program; call it once in a goroutine.
// Like RelayModule, there's nothing to sample on a timer - GetValues is
// a fast ioctl, read synchronously by ReadDigitalInput on demand - so
// this loop only opens or releases the line request as
// LiveEnabled/wiring changes.
func (m *DigitalInputModule) Run() {
	for {
		select {
		case <-m.stop:
			return
		default:
		}

		cfg := m.live.Snapshot().DigitalInputModule

		if !cfg.LiveEnabled {
			m.closeLines()
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if sig := digitalInputConfigSignature(cfg); sig != m.currentSignature() {
			if err := m.openLines(cfg); err != nil {
				log.Printf("Digital input module: %v", err)
				time.Sleep(1 * time.Second)
				continue
			}
			log.Printf("Digital input module: live data ON (%s)", cfg.Chip)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (m *DigitalInputModule) Stop() {
	close(m.stop)
	m.closeLines()
}

func (m *DigitalInputModule) currentSignature() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.openSig
}

func (m *DigitalInputModule) openLines(cfg DigitalInputModuleConfig) error {
	var offsets []int
	idx := map[DigitalInput]int{}
	for _, ch := range cfg.Channels {
		if !ch.Enabled {
			continue
		}
		idx[ch.Assigned] = len(offsets)
		offsets = append(offsets, ch.Port)
	}
	if len(offsets) == 0 {
		return fmt.Errorf("no digital input channels enabled")
	}

	lines, err := m.openFn(cfg.Chip, offsets, "hydrogen-booster")
	if err != nil {
		return fmt.Errorf("open %s: %w", cfg.Chip, err)
	}

	m.mu.Lock()
	if m.lines != nil {
		_ = m.lines.Close()
	}
	m.lines = lines
	m.portIndex = idx
	m.openSig = digitalInputConfigSignature(cfg)
	m.mu.Unlock()
	return nil
}

func (m *DigitalInputModule) closeLines() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lines != nil {
		_ = m.lines.Close()
		m.lines = nil
	}
	m.portIndex = nil
	m.openSig = ""
}

func (m *DigitalInputModule) assignedChannel(cfg DigitalInputModuleConfig, d DigitalInput) (int, bool) {
	for i, ch := range cfg.Channels {
		if ch.Enabled && ch.Assigned == d {
			return i, true
		}
	}
	return 0, false
}

// ReadDigitalInput returns d's current level if (and only if) it's
// assigned to an enabled channel on this module; ok is false otherwise,
// so CompositeIO can fall back to the base (simulated) IO for anything
// not wired to this module.
func (m *DigitalInputModule) ReadDigitalInput(d DigitalInput) (value bool, ok bool) {
	cfg := m.live.Snapshot().DigitalInputModule
	if _, assigned := m.assignedChannel(cfg, d); !assigned {
		return false, false
	}

	if cfg.LiveEnabled {
		m.mu.Lock()
		lines := m.lines
		idx, wired := m.portIndex[d]
		m.mu.Unlock()
		if lines == nil || !wired {
			return false, false
		}
		bits, err := lines.GetValues()
		if err != nil {
			return false, false
		}
		return bits&(uint64(1)<<uint(idx)) != 0, true
	}

	m.mu.Lock()
	v, ok := m.simValues[d]
	m.mu.Unlock()
	return v, ok
}

// SetSimValue adjusts one channel's simulated level in real time (e.g.
// from an admin.html checkbox). Returns an error if that signal isn't
// assigned to an enabled channel, or if the module is currently live
// (simulated values only apply while live data is off).
func (m *DigitalInputModule) SetSimValue(d DigitalInput, level bool) error {
	cfg := m.live.Snapshot().DigitalInputModule
	if cfg.LiveEnabled {
		return fmt.Errorf("cannot set a simulated value while live data is on")
	}
	if _, assigned := m.assignedChannel(cfg, d); !assigned {
		return fmt.Errorf("%s is not assigned to an enabled channel", d)
	}
	m.mu.Lock()
	m.simValues[d] = level
	m.mu.Unlock()
	return nil
}

type DigitalInputChannelStatus struct {
	Index    int    `json:"index"`
	Enabled  bool   `json:"enabled"`
	Assigned string `json:"assigned,omitempty"`
	Port     int    `json:"port"`
	High     bool   `json:"high"`
	Ok       bool   `json:"ok"`
}

type DigitalInputModuleStatus struct {
	LiveEnabled bool                        `json:"liveEnabled"`
	Channels    []DigitalInputChannelStatus `json:"channels"`
}

func (m *DigitalInputModule) Status() DigitalInputModuleStatus {
	cfg := m.live.Snapshot().DigitalInputModule
	st := DigitalInputModuleStatus{LiveEnabled: cfg.LiveEnabled}
	for i, ch := range cfg.Channels {
		cs := DigitalInputChannelStatus{Index: i, Enabled: ch.Enabled, Port: ch.Port}
		if ch.Enabled {
			cs.Assigned = ch.Assigned.String()
			cs.High, cs.Ok = m.ReadDigitalInput(ch.Assigned)
		}
		st.Channels = append(st.Channels, cs)
	}
	return st
}
