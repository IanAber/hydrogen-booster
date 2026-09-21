package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// fakeSerialPort is a minimal in-memory SerialPort for testing
// FlowModule without real hardware: Write records what was sent, Read
// serves canned response lines queued in advance (each auto-suffixed
// with \r if missing, matching the CR-terminated framing every real
// response uses).
type fakeSerialPort struct {
	mu        sync.Mutex
	written   []string
	responses []string
	readBuf   []byte
	readErr   error
	closed    bool
}

func newFakeSerialPort(responses ...string) *fakeSerialPort {
	return &fakeSerialPort{responses: responses}
}

func (f *fakeSerialPort) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, string(p))
	return len(p), nil
}

func (f *fakeSerialPort) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return 0, f.readErr
	}
	if len(f.readBuf) == 0 {
		if len(f.responses) == 0 {
			return 0, fmt.Errorf("fakeSerialPort: no more queued responses")
		}
		next := f.responses[0]
		f.responses = f.responses[1:]
		if !strings.HasSuffix(next, "\r") {
			next += "\r"
		}
		f.readBuf = []byte(next)
	}
	n := copy(p, f.readBuf)
	f.readBuf = f.readBuf[n:]
	return n, nil
}

func (f *fakeSerialPort) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeSerialPort) lastWritten() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.written) == 0 {
		return ""
	}
	return f.written[len(f.written)-1]
}

// activeFlowConfig returns a Config with the flow module switched on
// (both MassMeasurementEnabled and SensorType, per FlowModule's "two
// switches must agree" design) and RS-232 framing.
func activeFlowConfig() Config {
	cfg := DefaultConfig()
	cfg.MassMeasurementEnabled = true
	cfg.FlowModule.SensorType = FlowSensorAalborgXFM
	cfg.FlowModule.Port = "/dev/fake"
	cfg.FlowModule.BaudRate = 9600
	cfg.FlowModule.UseRS485 = false
	cfg.FlowModule.Unit = "kg/min"
	return cfg
}

func TestFlowModuleInactiveNeverOpensPort(t *testing.T) {
	none := DefaultConfig()
	none.MassMeasurementEnabled = true
	none.FlowModule.SensorType = FlowSensorNone

	massOff := activeFlowConfig()
	massOff.MassMeasurementEnabled = false

	for _, cfg := range []Config{none, massOff} {
		openCalls := 0
		live := NewLiveConfig(cfg)
		fm := NewFlowModule(live, func(path string, baud int) (SerialPort, error) {
			openCalls++
			return newFakeSerialPort(), nil
		})

		if _, ok := fm.ReadTotalKg(); ok {
			t.Errorf("ReadTotalKg ok=true while inactive (sensorType=%s, massEnabled=%v)",
				cfg.FlowModule.SensorType, cfg.MassMeasurementEnabled)
		}
		if handled, err := fm.ResetTotal(); handled || err != nil {
			t.Errorf("ResetTotal = (%v, %v), want (false, nil) while inactive", handled, err)
		}
		if openCalls != 0 {
			t.Errorf("openFn called %d times, want 0 while inactive", openCalls)
		}
	}
}

func TestFlowModuleConnectSendsUnitCommand(t *testing.T) {
	cfg := activeFlowConfig()
	live := NewLiveConfig(cfg)
	port := newFakeSerialPort("U:kg/min")
	fm := NewFlowModule(live, func(path string, baud int) (SerialPort, error) { return port, nil })

	if err := fm.ensureConnected(cfg.FlowModule); err != nil {
		t.Fatalf("ensureConnected: %v", err)
	}
	if got := port.lastWritten(); got != "U,kg/min\r" {
		t.Fatalf("unit command written = %q, want %q", got, "U,kg/min\r")
	}
}

func TestFlowModulePollOnceParsesTotal(t *testing.T) {
	cfg := activeFlowConfig()
	live := NewLiveConfig(cfg)
	port := newFakeSerialPort("U:kg/min", "3.142")
	fm := NewFlowModule(live, func(path string, baud int) (SerialPort, error) { return port, nil })

	if err := fm.ensureConnected(cfg.FlowModule); err != nil {
		t.Fatalf("ensureConnected: %v", err)
	}
	fm.pollOnce(cfg.FlowModule)

	v, ok := fm.ReadTotalKg()
	if !ok {
		t.Fatal("ReadTotalKg ok=false after a successful poll")
	}
	if v != 3.142 {
		t.Fatalf("ReadTotalKg = %v, want 3.142", v)
	}
}

func TestFlowModuleResetSendsCommandAndClearsCache(t *testing.T) {
	cfg := activeFlowConfig()
	live := NewLiveConfig(cfg)
	port := newFakeSerialPort("U:kg/min", "3.142", "TZ")
	fm := NewFlowModule(live, func(path string, baud int) (SerialPort, error) { return port, nil })

	if err := fm.ensureConnected(cfg.FlowModule); err != nil {
		t.Fatalf("ensureConnected: %v", err)
	}
	fm.pollOnce(cfg.FlowModule)
	if _, ok := fm.ReadTotalKg(); !ok {
		t.Fatal("expected a cached total before Reset")
	}

	handled, err := fm.ResetTotal()
	if !handled || err != nil {
		t.Fatalf("ResetTotal = (%v, %v), want (true, nil)", handled, err)
	}
	if got := port.lastWritten(); got != "T,Z\r" {
		t.Fatalf("reset command written = %q, want %q", got, "T,Z\r")
	}
	if _, ok := fm.ReadTotalKg(); ok {
		t.Fatal("ReadTotalKg ok=true after Reset - cached total should be cleared")
	}
}

func TestFlowModuleRS485FramingAddsAndStripsAddress(t *testing.T) {
	cfg := activeFlowConfig()
	cfg.FlowModule.UseRS485 = true
	cfg.FlowModule.Address = 0x12
	live := NewLiveConfig(cfg)
	port := newFakeSerialPort("!12,U:kg/min", "!12,3.142")
	fm := NewFlowModule(live, func(path string, baud int) (SerialPort, error) { return port, nil })

	if err := fm.ensureConnected(cfg.FlowModule); err != nil {
		t.Fatalf("ensureConnected: %v", err)
	}
	if got := port.lastWritten(); got != "!12,U,kg/min\r" {
		t.Fatalf("unit command written = %q, want %q", got, "!12,U,kg/min\r")
	}

	fm.pollOnce(cfg.FlowModule)
	if got := port.lastWritten(); got != "!12,T,R\r" {
		t.Fatalf("poll command written = %q, want %q", got, "!12,T,R\r")
	}
	v, ok := fm.ReadTotalKg()
	if !ok || v != 3.142 {
		t.Fatalf("ReadTotalKg = (%v, %v), want (3.142, true) - RS-485 prefix should be stripped before parsing", v, ok)
	}
}

func TestFlowModuleMalformedResponseLeavesNotOk(t *testing.T) {
	cfg := activeFlowConfig()
	live := NewLiveConfig(cfg)
	port := newFakeSerialPort("U:kg/min", "ERR")
	fm := NewFlowModule(live, func(path string, baud int) (SerialPort, error) { return port, nil })

	if err := fm.ensureConnected(cfg.FlowModule); err != nil {
		t.Fatalf("ensureConnected: %v", err)
	}
	fm.pollOnce(cfg.FlowModule)

	if _, ok := fm.ReadTotalKg(); ok {
		t.Fatal("ReadTotalKg ok=true after a malformed (non-numeric) totalizer response")
	}
}

func TestFlowModuleReadErrorLeavesNotOkWithoutPanicking(t *testing.T) {
	cfg := activeFlowConfig()
	live := NewLiveConfig(cfg)
	port := newFakeSerialPort("U:kg/min")
	fm := NewFlowModule(live, func(path string, baud int) (SerialPort, error) { return port, nil })

	if err := fm.ensureConnected(cfg.FlowModule); err != nil {
		t.Fatalf("ensureConnected: %v", err)
	}
	// Simulate a disconnected/unresponsive device: serialport_linux.go's
	// real Read() turns a VTIME timeout into exactly this kind of error
	// rather than looping forever - see its doc comment.
	port.readErr = fmt.Errorf("serial read timeout")
	fm.pollOnce(cfg.FlowModule)

	if _, ok := fm.ReadTotalKg(); ok {
		t.Fatal("ReadTotalKg ok=true after a read error")
	}
}
