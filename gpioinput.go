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
	// liveSince is when live data was switched on or the wiring last
	// changed (zero while in simulation); lastOpenErr is why the lines
	// last failed to open - see readForIO.
	liveSince   time.Time
	lastOpenErr string
	errLog      errorLatch

	stop chan struct{}
}

// NewDigitalInputModule constructs a module against live config. openFn
// opens a group of GPIO lines as inputs - pass OpenDigitalInputLines for
// the real driver; tests pass a fake instead.
func NewDigitalInputModule(live *LiveConfig, openFn func(chipPath string, offsets []int, consumer string) (DigitalInputLines, error)) *DigitalInputModule {
	return &DigitalInputModule{
		live:   live,
		openFn: openFn,
		// HydraulicOilLevel defaults to false (0/low = sufficient oil).
		// Motor1Powered/Motor2Powered start true (high = confirmed
		// stopped, matching both pumps starting off) - the same
		// defaults SimulatedIO seeds - so the admin screen's live
		// panel reads correctly before any pump command has ever
		// mirrored a value in here (see CompositeIO.SetRelay) or an
		// admin has touched a sim value themselves.
		simValues: map[DigitalInput]bool{Motor1Powered: true, Motor2Powered: true},
		stop:      make(chan struct{}),
	}
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
	pendingSig := "" // the wiring signature liveSince was last reset for
	for {
		select {
		case <-m.stop:
			return
		default:
		}

		cfg := m.live.Snapshot().DigitalInputModule

		if !cfg.LiveEnabled {
			m.closeLines()
			m.mu.Lock()
			m.liveSince = time.Time{}
			m.mu.Unlock()
			pendingSig = ""
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if sig := digitalInputConfigSignature(cfg); sig != m.currentSignature() {
			if sig != pendingSig {
				pendingSig = sig
				m.mu.Lock()
				m.liveSince = time.Now()
				m.mu.Unlock()
			}
			if err := m.openLines(cfg); err != nil {
				m.errLog.report("Digital input module", err)
				m.mu.Lock()
				m.lastOpenErr = err.Error()
				m.mu.Unlock()
				time.Sleep(1 * time.Second)
				continue
			}
			m.mu.Lock()
			m.lastOpenErr = ""
			m.mu.Unlock()
			m.errLog.report("Digital input module", nil)
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

	// Release any existing line request before asking for the new one -
	// the new set of enabled lines usually overlaps the old set (e.g.
	// adding one more channel keeps every previously-enabled line too),
	// and the kernel refuses to grant a second exclusive request for a
	// line this same handle is still holding open.
	m.mu.Lock()
	oldLines := m.lines
	m.lines = nil
	m.mu.Unlock()
	if oldLines != nil {
		_ = oldLines.Close()
	}

	lines, err := m.openFn(cfg.Chip, offsets, "hydrogen-booster")
	if err != nil {
		return fmt.Errorf("open %s: %w", cfg.Chip, err)
	}

	m.mu.Lock()
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

// readForIO is CompositeIO's view of a reading - see
// PressureModule.readForIO; while live, every digital input is this
// module's responsibility. GPIO reads are synchronous, so there's no
// staleness - only a short grace period while the lines are (re)opened.
func (m *DigitalInputModule) readForIO(d DigitalInput) (value bool, handled bool, err error) {
	snap := m.live.Snapshot()
	cfg := snap.DigitalInputModule
	if !cfg.LiveEnabled {
		v, ok := m.ReadDigitalInput(d)
		return v, ok, nil
	}
	if _, assigned := m.assignedChannel(cfg, d); !assigned {
		return false, true, msgErr("not_assigned_digital", nil, "not assigned to an enabled digital input channel")
	}
	m.mu.Lock()
	lines := m.lines
	idx, wired := m.portIndex[d]
	liveSince, lastOpenErr := m.liveSince, m.lastOpenErr
	m.mu.Unlock()
	if lines == nil || !wired {
		grace := sensorMaxAge(snap.Safety.SensorMaxAge, time.Second)
		if liveSince.IsZero() || time.Since(liveSince) < grace {
			return false, true, ErrSensorPending
		}
		return false, true, withLastErr(msg("gpio_not_open", map[string]any{"chip": cfg.Chip}, "GPIO input not open on %s", cfg.Chip), lastOpenErr)
	}
	bits, err := lines.GetValues()
	if err != nil {
		return false, true, &msgError{Message: msg("gpio_read_failed", map[string]any{"detail": err.Error()}, "GPIO read: %v", err), wrapped: err}
	}
	return bits&(uint64(1)<<uint(idx)) != 0, true, nil
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
