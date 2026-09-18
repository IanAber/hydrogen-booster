package main

import (
	"errors"
	"math/rand"
	"sync"
	"time"
)

// RelayOutput enumerates every relay-driven output on the skid.
type RelayOutput int

const (
	Valve1A   RelayOutput = iota // Stage 1 direction valve (energise = up stroke)
	Valve1B                      // Stage 1 secondary valve, pulsed ~500ms on direction change
	Valve2A                      // Stage 2 direction valve
	Valve2B                      // Stage 2 secondary valve, pulsed ~500ms on direction change
	VentValve                    // Bleeds the delivery hose after a 700 bar fill
)

func (r RelayOutput) String() string {
	switch r {
	case Valve1A:
		return "Valve1A(dir)"
	case Valve1B:
		return "Valve1B(pulse)"
	case Valve2A:
		return "Valve2A(dir)"
	case Valve2B:
		return "Valve2B(pulse)"
	case VentValve:
		return "VentValve"
	default:
		return "unknown"
	}
}

// ParseRelayOutput is the inverse of RelayOutput.String(), used when
// reading a valve name back out of the config file or an admin API
// request (see RelayChannelConfig).
func ParseRelayOutput(s string) (RelayOutput, bool) {
	switch s {
	case "Valve1A(dir)":
		return Valve1A, true
	case "Valve1B(pulse)":
		return Valve1B, true
	case "Valve2A(dir)":
		return Valve2A, true
	case "Valve2B(pulse)":
		return Valve2B, true
	case "VentValve":
		return VentValve, true
	default:
		return 0, false
	}
}

// SensorInput enumerates every pressure/temperature transmitter on the skid.
type SensorInput int

const (
	Hyd1Pressure SensorInput = iota
	Hyd1Temp
	Hyd2Pressure
	Hyd2Temp
	TankPressure // hydrogen storage tank bank
	TankTemp
	InterStagePressure // between stage 1 output (B) and stage 2 input (C)
	InterStageTemp
	OutputPressure // final delivery, at the nozzle
	OutputTemp
)

// String gives each sensor a stable, human-readable name - used in the
// JSON config file and the admin screen's channel-assignment dropdown,
// so config files stay readable and don't depend on the numeric enum
// order in this source file.
func (s SensorInput) String() string {
	switch s {
	case Hyd1Pressure:
		return "Hyd1Pressure"
	case Hyd1Temp:
		return "Hyd1Temp"
	case Hyd2Pressure:
		return "Hyd2Pressure"
	case Hyd2Temp:
		return "Hyd2Temp"
	case TankPressure:
		return "TankPressure"
	case TankTemp:
		return "TankTemp"
	case InterStagePressure:
		return "InterStagePressure"
	case InterStageTemp:
		return "InterStageTemp"
	case OutputPressure:
		return "OutputPressure"
	case OutputTemp:
		return "OutputTemp"
	default:
		return "Unknown"
	}
}

// ParseSensorInput is the inverse of String(), used when reading a
// sensor name back out of the config file or an admin API request.
func ParseSensorInput(s string) (SensorInput, bool) {
	switch s {
	case "Hyd1Pressure":
		return Hyd1Pressure, true
	case "Hyd1Temp":
		return Hyd1Temp, true
	case "Hyd2Pressure":
		return Hyd2Pressure, true
	case "Hyd2Temp":
		return Hyd2Temp, true
	case "TankPressure":
		return TankPressure, true
	case "TankTemp":
		return TankTemp, true
	case "InterStagePressure":
		return InterStagePressure, true
	case "InterStageTemp":
		return InterStageTemp, true
	case "OutputPressure":
		return OutputPressure, true
	case "OutputTemp":
		return OutputTemp, true
	default:
		return 0, false
	}
}

// isPressureSensor restricts CAN pressure-module channel assignment to
// actual pressure readings (the module is a 4-20mA pressure-loop input
// module; assigning it to a temperature reading would be a
// configuration mistake worth rejecting up front).
func isPressureSensor(s SensorInput) bool {
	switch s {
	case Hyd1Pressure, Hyd2Pressure, TankPressure, InterStagePressure, OutputPressure:
		return true
	default:
		return false
	}
}

// isTempSensor restricts the 1-Wire temperature module's channel
// assignment to actual temperature readings.
func isTempSensor(s SensorInput) bool {
	switch s {
	case Hyd1Temp, Hyd2Temp, TankTemp, InterStageTemp, OutputTemp:
		return true
	default:
		return false
	}
}

// ValidLanguages are the web UI language codes the admin screen's
// language selector offers, and the only values Config.Language may
// take. The web UI (web/i18n.js) has a translation table for each of
// these; adding a language means adding it both here and there.
var ValidLanguages = map[string]bool{
	"en": true, "es": true, "fr": true, "de": true, "pt": true,
}

// IO is the hardware abstraction boundary. Everything above this
// interface (Stage, Controller, WebServer) is hardware-agnostic. To
// deploy on the real skid, write an implementation of IO that talks to
// your actual relay/IO hardware (e.g. Modbus TCP/RTU remote IO, EtherCAT,
// or direct GPIO) and pass it into NewController instead of SimulatedIO.
type IO interface {
	SetRelay(r RelayOutput, on bool) error
	RelayState(r RelayOutput) (bool, error)
	ReadPressureBar(s SensorInput) (float64, error)
	ReadTempC(s SensorInput) (float64, error)
	Close() error
}

// RunnableIO is an optional capability an IO implementation can support:
// being told explicitly when a fill job is actively running versus
// idle/complete/fault. Real hardware doesn't need this - pressure
// genuinely doesn't move unless the pump is actually pumping - but
// SimulatedIO's physics model has no such natural gate, so Controller
// drives it explicitly (see setIORunning in controller.go) to stop
// simulated readings drifting forever in the background between jobs.
type RunnableIO interface {
	SetRunning(running bool)
}

var ErrSensorFault = errors.New("sensor read fault")

// SimulatedIO is a lightweight stand-in physics model, good enough to
// exercise the control logic, synchronisation and web UI without real
// hardware attached. It is NOT a substitute for commissioning against the
// real machine.
type SimulatedIO struct {
	mu      sync.Mutex
	relays  map[RelayOutput]bool
	running bool // see RunnableIO; gates the physics below so it doesn't drift forever between jobs

	hyd1P, hyd2P float64
	outP         float64
	tankP        float64
	temps        map[SensorInput]float64

	stop chan struct{}
}

func NewSimulatedIO() *SimulatedIO {
	s := &SimulatedIO{
		relays: map[RelayOutput]bool{},
		hyd1P:  20, hyd2P: 20,
		outP:  5,
		tankP: 200,
		temps: map[SensorInput]float64{},
		stop:  make(chan struct{}),
	}
	go s.run()
	return s
}

func (s *SimulatedIO) run() {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.step()
		}
	}
}

// step crudely mimics hydraulic pressure climbing as a ram drives into the
// gas column (reset to a low value whenever the controller flips a
// direction valve, i.e. starts a fresh stroke), the output pressure
// creeping up while both stages are cycling, and it bleeding down when
// the vent valve is open. The climbing/creeping/draining parts only
// happen while s.running is true (see SetRunning) - otherwise, exactly
// like real hardware with the pump off, nothing here should be moving at
// all between jobs, no matter how long the process has been up.
func (s *SimulatedIO) step() {
	s.mu.Lock()
	defer s.mu.Unlock()
	jitter := func() float64 { return (rand.Float64() - 0.5) * 0.6 }

	if s.running {
		s.hyd1P += 1.2 + jitter()
		s.hyd2P += 1.4 + jitter()
		if s.hyd1P > 220 {
			s.hyd1P = 220
		}
		if s.hyd2P > 220 {
			s.hyd2P = 220
		}

		if s.outP < 720 {
			s.outP += 0.08
		}
		if s.tankP > 5 {
			s.tankP -= 0.002
		}
	} else if s.outP > 0 {
		// Release the delivery hose pressure back toward atmospheric
		// once a fill stops, regardless of nozzle. On a real 700 bar
		// fill this already happens because Controller actually opens
		// the vent valve; there's no equivalent real step for 350 bar
		// (the nozzle just disconnects), so this simulates the same
		// release in test mode for both, rather than leaving the
		// simulated reading stuck wherever the fill ended.
		s.outP -= 3
		if s.outP < 0 {
			s.outP = 0
		}
	}
	s.temps[Hyd1Temp] = 40 + jitter()
	s.temps[Hyd2Temp] = 42 + jitter()
	s.temps[TankTemp] = 15 + jitter()*0.2
	s.temps[InterStageTemp] = 25 + jitter()
	s.temps[OutputTemp] = 30 + jitter()
}

// SetRunning implements RunnableIO - see that interface's doc comment.
func (s *SimulatedIO) SetRunning(running bool) {
	s.mu.Lock()
	s.running = running
	s.mu.Unlock()
}

func (s *SimulatedIO) SetRelay(r RelayOutput, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	wasOn := s.relays[r]
	s.relays[r] = on
	// A direction-valve change resets the corresponding hydraulic
	// pressure, simulating the ram starting a fresh stroke.
	if r == Valve1A && wasOn != on {
		s.hyd1P = 15
	}
	if r == Valve2A && wasOn != on {
		s.hyd2P = 15
	}
	return nil
}

func (s *SimulatedIO) RelayState(r RelayOutput) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.relays[r], nil
}

func (s *SimulatedIO) ReadPressureBar(sn SensorInput) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch sn {
	case Hyd1Pressure:
		return s.hyd1P, nil
	case Hyd2Pressure:
		return s.hyd2P, nil
	case TankPressure:
		return s.tankP, nil
	case OutputPressure:
		return s.outP, nil
	case InterStagePressure:
		return s.outP * 0.5, nil
	default:
		return 0, ErrSensorFault
	}
}

func (s *SimulatedIO) ReadTempC(sn SensorInput) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.temps[sn]; ok {
		return v, nil
	}
	return 20, nil
}

func (s *SimulatedIO) Close() error {
	close(s.stop)
	return nil
}
