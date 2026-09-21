package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// GpioLines abstracts one open line-request handle from an ioctl-driven
// gpiochip - a group of GPIO lines reserved together as outputs,
// addressed by their position within the request (0..n-1), all
// read/written through the same file descriptor. OpenGpioLines
// (gpiochip_linux.go) is the real implementation; tests pass a fake.
type GpioLines interface {
	// SetValues updates the lines selected by mask (bit i = line i in
	// this request) to the corresponding bits in bits; lines outside
	// mask are left untouched.
	SetValues(mask, bits uint64) error
	// GetValues returns the current level of every line in this
	// request, bit i = line i.
	GetValues() (uint64, error)
	Close() error
}

// RelayModule drives valve relays through the MCP23017 GPIO expander,
// addressed via the kernel gpiochip character device. It mirrors
// PressureModule/TemperatureModule's live switch: while
// RelayModule.LiveEnabled is on, SetRelay/RelayState for an assigned,
// enabled channel go to the real GPIO expander; otherwise (or for any
// valve not assigned to an enabled channel) they report unhandled, so
// CompositeIO falls back to the in-memory simulator. Starts disabled so
// nothing drives real relays until an operator has confirmed port
// wiring from the admin screen.
type RelayModule struct {
	live   *LiveConfig
	openFn func(chipPath string, offsets []int, consumer string) (GpioLines, error)

	mu        sync.Mutex
	lines     GpioLines
	openSig   string // config signature the currently-open `lines` was opened for
	portIndex map[RelayOutput]int

	stop chan struct{}
}

// NewRelayModule constructs a module against live config. openFn opens
// a group of GPIO lines as outputs - pass OpenGpioLines for the real
// driver; tests pass a fake instead.
func NewRelayModule(live *LiveConfig, openFn func(chipPath string, offsets []int, consumer string) (GpioLines, error)) *RelayModule {
	return &RelayModule{live: live, openFn: openFn, stop: make(chan struct{})}
}

// relayConfigSignature changes whenever the chip path or the set of
// enabled channel->port assignments changes, so Run() knows to
// re-request the GPIO lines rather than silently keep driving a stale
// mapping after the operator edits it.
func relayConfigSignature(cfg RelayModuleConfig) string {
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
// Unlike the sensor modules there's nothing to sample on a timer - GPIO
// output state is set on demand by SetRelay - so this loop only opens or
// releases the line request as LiveEnabled/wiring changes.
func (m *RelayModule) Run() {
	for {
		select {
		case <-m.stop:
			return
		default:
		}

		cfg := m.live.Snapshot().RelayModule

		if !cfg.LiveEnabled {
			m.closeLines()
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if sig := relayConfigSignature(cfg); sig != m.currentSignature() {
			if err := m.openLines(cfg); err != nil {
				log.Printf("Relay module: %v", err)
				time.Sleep(1 * time.Second)
				continue
			}
			log.Printf("Relay module: live data ON (%s)", cfg.Chip)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (m *RelayModule) Stop() {
	close(m.stop)
	m.closeLines()
}

func (m *RelayModule) currentSignature() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.openSig
}

func (m *RelayModule) openLines(cfg RelayModuleConfig) error {
	var offsets []int
	idx := map[RelayOutput]int{}
	for _, ch := range cfg.Channels {
		if !ch.Enabled {
			continue
		}
		idx[ch.Assigned] = len(offsets)
		offsets = append(offsets, ch.Port)
	}
	if len(offsets) == 0 {
		return fmt.Errorf("no relay channels enabled")
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
	m.openSig = relayConfigSignature(cfg)
	m.mu.Unlock()
	return nil
}

func (m *RelayModule) closeLines() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lines != nil {
		_ = m.lines.Close()
		m.lines = nil
	}
	m.portIndex = nil
	m.openSig = ""
}

// SetRelay drives r if (and only if) it's assigned to an enabled channel
// and the module is live; handled is false otherwise, so CompositeIO
// knows to fall back to the in-memory simulator.
func (m *RelayModule) SetRelay(r RelayOutput, on bool) (handled bool, err error) {
	m.mu.Lock()
	lines := m.lines
	idx, assigned := m.portIndex[r]
	m.mu.Unlock()
	if lines == nil || !assigned {
		return false, nil
	}
	bit := uint64(1) << uint(idx)
	bits := uint64(0)
	if on {
		bits = bit
	}
	return true, lines.SetValues(bit, bits)
}

// RelayState reads back r's current level if it's assigned to an enabled
// channel and the module is live; handled is false otherwise.
func (m *RelayModule) RelayState(r RelayOutput) (on bool, handled bool, err error) {
	m.mu.Lock()
	lines := m.lines
	idx, assigned := m.portIndex[r]
	m.mu.Unlock()
	if lines == nil || !assigned {
		return false, false, nil
	}
	bits, err := lines.GetValues()
	if err != nil {
		return false, true, err
	}
	return bits&(uint64(1)<<uint(idx)) != 0, true, nil
}
