package main

import (
	"bufio"
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

// serialPortsByIDPath is a var, not a const, so tests can point it at a
// temporary directory instead of the real udev-managed tree (mirrors
// w1DevicesPath in onewire.go).
var serialPortsByIDPath = "/dev/serial/by-id"

// DiscoverSerialPorts lists the stable, human-readable serial device
// paths under /dev/serial/by-id (populated by udev, and stable across
// reboots/re-plugging - unlike the raw /dev/ttyUSBN device nodes, whose
// numbering depends on plug order) - for the admin screen's port
// picker. Returns an empty slice (not an error) if the directory
// doesn't exist - off-target development, or no USB-serial adapter has
// ever been plugged in - rather than failing the whole config load
// over it (mirrors DiscoverOneWireDevices in onewire.go).
func DiscoverSerialPorts() []string {
	entries, err := os.ReadDir(serialPortsByIDPath)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, filepath.Join(serialPortsByIDPath, e.Name()))
	}
	sort.Strings(paths)
	return paths
}

// SerialPort abstracts one open serial connection - OpenSerialPort
// (serialport_linux.go) is the real implementation; tests pass a fake.
// Its method set is structurally an io.ReadWriteCloser, so a SerialPort
// value can be wrapped directly in a bufio.Reader for line-based
// response reads.
type SerialPort interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
}

// FlowModule talks to an optional real mass flow sensor over a serial
// link - currently only the Aalborg XFM series' ASCII command protocol
// (9600 baud, 8N1, no flow control; comma-delimited arguments,
// CR-terminated - see the XFM operating manual's "RS-232/RS-485
// Software Interface Commands" section). It mirrors RelayModule's
// connect/reconnect-on-config-change shape, but is active only when
// BOTH Config.MassMeasurementEnabled and FlowModuleConfig.SensorType
// say so - see FlowModuleConfig's doc comment in config.go for why
// there's no separate LiveEnabled field the way the other modules have
// one.
//
// The XFM has a built-in totalizer with mass-unit support, so no
// software flow-rate integration is needed: ResetTotal zeroes it
// ("T,Z"), the poll loop reads it back ("T,R"). Its unit resets to "%
// full scale" on every power cycle, so the configured Unit is
// (re-)sent on every (re)connect and whenever it's changed.
type FlowModule struct {
	live   *LiveConfig
	openFn func(path string, baud int) (SerialPort, error)

	// portMu serializes every command/response round trip over the
	// port - both the poll loop and a synchronous ResetTotal() call
	// (from Controller.Start(), a different goroutine) share the one
	// connection and its one bufio.Reader.
	portMu   sync.Mutex
	port     SerialPort
	reader   *bufio.Reader
	connCfg  FlowModuleConfig // the connection-relevant fields the currently-open port was opened for
	sentUnit string           // last unit string successfully acknowledged by the device

	mu          sync.RWMutex
	haveTotal   bool
	lastTotalKg float64
	lastErr     string
	lastUpdate  time.Time

	stop chan struct{}
}

// NewFlowModule constructs a module against live config. openFn opens
// the serial port - pass OpenSerialPort for the real driver; tests pass
// a fake instead.
func NewFlowModule(live *LiveConfig, openFn func(path string, baud int) (SerialPort, error)) *FlowModule {
	return &FlowModule{live: live, openFn: openFn, stop: make(chan struct{})}
}

// flowModuleActive reports whether cfg says the flow module should be
// connected right now - both switches (see FlowModuleConfig's doc
// comment) must agree.
func flowModuleActive(cfg Config) bool {
	return cfg.MassMeasurementEnabled && cfg.FlowModule.SensorType == FlowSensorAalborgXFM
}

// Run polls for the life of the program; call it once in a goroutine.
func (m *FlowModule) Run() {
	wasActive := false
	for {
		select {
		case <-m.stop:
			return
		default:
		}

		cfg := m.live.Snapshot()
		if !flowModuleActive(cfg) {
			if wasActive {
				m.closePort()
				log.Printf("Flow module: inactive (mass measurement off or no sensor selected)")
			}
			wasActive = false
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if err := m.ensureConnected(cfg.FlowModule); err != nil {
			log.Printf("Flow module: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}
		if !wasActive {
			log.Printf("Flow module: live data ON (%s)", cfg.FlowModule.Port)
		}
		wasActive = true

		m.pollOnce(cfg.FlowModule)
		time.Sleep(cfg.FlowModule.PollPeriod)
	}
}

func (m *FlowModule) Stop() {
	close(m.stop)
	m.closePort()
}

// connectionParams is the subset of FlowModuleConfig that requires
// actually closing and reopening the serial port when it changes -
// Unit doesn't (see ensureUnit), and PollPeriod just changes how often
// Run's loop calls pollOnce.
type connectionParams struct {
	Port     string
	BaudRate int
	UseRS485 bool
	Address  int
}

func connParams(cfg FlowModuleConfig) connectionParams {
	return connectionParams{Port: cfg.Port, BaudRate: cfg.BaudRate, UseRS485: cfg.UseRS485, Address: cfg.Address}
}

// ensureConnected (re)opens the serial port if the connection-relevant
// settings have changed (or it's not open yet), then makes sure the
// device's active unit matches cfg.Unit.
func (m *FlowModule) ensureConnected(cfg FlowModuleConfig) error {
	m.portMu.Lock()
	needsReconnect := m.port == nil || connParams(m.connCfg) != connParams(cfg)
	m.portMu.Unlock()

	if needsReconnect {
		if err := m.reconnect(cfg); err != nil {
			return err
		}
	}
	return m.ensureUnit(cfg)
}

func (m *FlowModule) reconnect(cfg FlowModuleConfig) error {
	port, err := m.openFn(cfg.Port, cfg.BaudRate)
	if err != nil {
		return fmt.Errorf("open %s: %w", cfg.Port, err)
	}
	m.portMu.Lock()
	if m.port != nil {
		_ = m.port.Close()
	}
	m.port = port
	m.reader = bufio.NewReader(port)
	m.connCfg = cfg
	m.sentUnit = ""
	m.portMu.Unlock()
	return nil
}

// ensureUnit (re)sends the unit-select command whenever cfg.Unit
// differs from what the device last acknowledged - the XFM's unit
// setting resets to "% full scale" on every power cycle, so it can't
// just be sent once at connect time and forgotten if an operator
// changes it later without also changing Port/BaudRate (which would
// otherwise be the only thing that triggers a reconnect).
func (m *FlowModule) ensureUnit(cfg FlowModuleConfig) error {
	m.portMu.Lock()
	alreadySet := m.sentUnit == cfg.Unit
	m.portMu.Unlock()
	if alreadySet {
		return nil
	}
	resp, err := m.doCommand(cfg, fmt.Sprintf("U,%s", cfg.Unit))
	if err != nil {
		return fmt.Errorf("set unit: %w", err)
	}
	// Expected "U:<unit>" - tolerate minor formatting differences
	// rather than hard-failing over an ack format quirk this can't be
	// verified against real hardware for.
	if !strings.Contains(resp, cfg.Unit) {
		log.Printf("Flow module: unexpected response to unit select: %q", resp)
	}
	m.portMu.Lock()
	m.sentUnit = cfg.Unit
	m.portMu.Unlock()
	return nil
}

func (m *FlowModule) closePort() {
	m.portMu.Lock()
	defer m.portMu.Unlock()
	if m.port != nil {
		_ = m.port.Close()
		m.port = nil
	}
	m.reader = nil
	m.connCfg = FlowModuleConfig{}
	m.sentUnit = ""
}

// frame builds the wire representation of cmd per the Aalborg XFM
// manual's "Commands Structure": RS-485 prefixes a start character and
// a 2-hex-digit address; RS-232 omits both.
func frame(cfg FlowModuleConfig, cmd string) string {
	if cfg.UseRS485 {
		return fmt.Sprintf("!%02X,%s\r", cfg.Address, cmd)
	}
	return cmd + "\r"
}

// parseResponse strips the RS-485 echo prefix (if any) from a raw
// response line, returning the bare payload.
func parseResponse(cfg FlowModuleConfig, raw string) string {
	raw = strings.TrimSpace(raw)
	if cfg.UseRS485 {
		prefix := fmt.Sprintf("!%02X,", cfg.Address)
		raw = strings.TrimPrefix(raw, prefix)
	}
	return raw
}

// doCommand sends cmd and returns the parsed response payload. Callers
// must hold no other lock - it takes portMu itself, serializing against
// any concurrent caller (the poll loop or a synchronous ResetTotal()).
// A real SerialPort's Read is expected to return an error (rather than
// looping forever) once its underlying per-call read timeout elapses
// with nothing received - see serialport_linux.go - so an unresponsive
// or disconnected device surfaces here as an error within about a
// second, not a hang.
func (m *FlowModule) doCommand(cfg FlowModuleConfig, cmd string) (string, error) {
	m.portMu.Lock()
	defer m.portMu.Unlock()
	if m.port == nil || m.reader == nil {
		return "", fmt.Errorf("not connected")
	}
	if _, err := m.port.Write([]byte(frame(cfg, cmd))); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	line, err := m.reader.ReadString('\r')
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return parseResponse(cfg, line), nil
}

func (m *FlowModule) pollOnce(cfg FlowModuleConfig) {
	resp, err := m.doCommand(cfg, "T,R")
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.lastErr = err.Error()
		return
	}
	v, perr := strconv.ParseFloat(resp, 64)
	if perr != nil {
		m.lastErr = fmt.Sprintf("unexpected totalizer response %q", resp)
		return
	}
	m.haveTotal = true
	m.lastTotalKg = v
	m.lastErr = ""
	m.lastUpdate = time.Now()
}

// ReadTotalKg returns the last successfully polled totalizer value; ok
// is false if the module isn't active (mass measurement off, or no/a
// different sensor selected), hasn't connected yet, or its last poll
// errored. Controller falls back to the tank pressure-differential
// estimate in every such case (see Controller.deliveredMassKg) -
// exactly as if FlowSensorNone were selected, deliberately with no
// simulated placeholder value in between.
func (m *FlowModule) ReadTotalKg() (value float64, ok bool) {
	if !flowModuleActive(m.live.Snapshot()) {
		return 0, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.haveTotal || m.lastErr != "" {
		return 0, false
	}
	return m.lastTotalKg, true
}

// ResetTotal sends the totalizer reset command; handled is false if the
// module isn't currently active, so CompositeIO/Controller know this
// was a no-op rather than a real failure.
func (m *FlowModule) ResetTotal() (handled bool, err error) {
	cfg := m.live.Snapshot()
	if !flowModuleActive(cfg) {
		return false, nil
	}
	resp, err := m.doCommand(cfg.FlowModule, "T,Z")
	if err != nil {
		return true, err
	}
	if !strings.Contains(resp, "TZ") {
		return true, fmt.Errorf("unexpected reset response %q", resp)
	}
	m.mu.Lock()
	m.haveTotal = false
	m.lastTotalKg = 0
	m.lastErr = ""
	m.mu.Unlock()
	return true, nil
}

// FlowModuleStatus reports the flow module's state for the admin
// screen/diagnostics - see webserver.go's handleFlowStatus.
type FlowModuleStatus struct {
	SensorType       string     `json:"sensorType"`
	Active           bool       `json:"active"`
	LastUpdate       *time.Time `json:"lastUpdate,omitempty"`
	TotalKg          float64    `json:"totalKg"`
	Ok               bool       `json:"ok"`
	Error            string     `json:"error,omitempty"`
	AvailableDevices []string   `json:"availableDevices"`
}

func (m *FlowModule) Status() FlowModuleStatus {
	cfg := m.live.Snapshot()
	st := FlowModuleStatus{
		SensorType:       string(cfg.FlowModule.SensorType),
		Active:           flowModuleActive(cfg),
		AvailableDevices: DiscoverSerialPorts(),
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.lastUpdate.IsZero() {
		t := m.lastUpdate
		st.LastUpdate = &t
	}
	st.TotalKg = m.lastTotalKg
	st.Ok = m.haveTotal && m.lastErr == ""
	st.Error = m.lastErr
	return st
}
