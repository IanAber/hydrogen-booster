package main

import "time"

// StageConfig tunes one hydraulic circuit (Stage 1 = direction valve A +
// relief valve B, Stage 2 = direction valve C + relief valve D). A/C
// hold whatever direction they're commanded to, but the ram can't
// physically follow that command while the circuit is under pressure -
// the relief valve must be briefly opened (see Stages in stage.go) for
// the direction change to actually take mechanical effect. A stroke
// ends on whichever comes first: MaxStrokeTime elapses, or hydraulic
// pressure rises by SpikeDeltaBar within SpikeWindow (the signature of
// the ram stalling against the gas column at the end of a stroke).
type StageConfig struct {
	Name              string
	DirRelay          RelayOutput // A or C
	ReliefRelay       RelayOutput // B or D
	HydPressureSensor SensorInput

	MinStrokeTime time.Duration // ignore spike detection until this long into the stroke (debounce)
	MaxStrokeTime time.Duration // hard timeout, ends the stroke regardless of pressure
	SpikeDeltaBar float64       // pressure rise over SpikeWindow that counts as "end of stroke"
	SpikeWindow   time.Duration
	ReverseDelay  time.Duration // gap between commanding the new direction and re-closing the relief valve, letting the direction valve mechanically settle
}

// HomingConfig controls the startup sequence that gets both cylinders
// to the same physical reference position before the first stroke:
// both relief valves open and both direction valves forced off for
// Duration, so neither ram is driven and both settle to the same end
// regardless of where they were left by a previous run.
type HomingConfig struct {
	Duration time.Duration
}

// SafetyConfig holds interlock limits, independent of any single stage.
type SafetyConfig struct {
	MaxHydraulicPressureBar float64
	MaxHydrogenPressureBar  float64
	MaxTempC                float64
	SensorMaxAge            time.Duration
}

// CanChannelConfig is the calibration and assignment for one physical
// channel on the 6-channel 4-20mA CAN pressure input module: which
// logical pressure reading this channel feeds, and the two-point linear
// calibration (raw counts -> bar) for it.
type CanChannelConfig struct {
	Enabled    bool
	Assigned   SensorInput // meaningful only when Enabled; must be a pressure sensor
	CalLowRaw  int         // raw counts (0-65535) at the low calibration point
	CalLowBar  float64     // pressure (bar) at the low calibration point
	CalHighRaw int         // raw counts (0-65535) at the high calibration point
	CalHighBar float64     // pressure (bar) at the high calibration point
}

// ToBar linearly interpolates/extrapolates a raw reading to bar using
// this channel's two-point calibration.
func (c CanChannelConfig) ToBar(raw int) float64 {
	if c.CalHighRaw == c.CalLowRaw {
		return c.CalLowBar
	}
	frac := float64(raw-c.CalLowRaw) / float64(c.CalHighRaw-c.CalLowRaw)
	return c.CalLowBar + frac*(c.CalHighBar-c.CalLowBar)
}

// CanChannelCount is fixed at 6, matching the physical module (a 6
// channel 4-20mA input module with isolated CAN bus communications).
const CanChannelCount = 6

// CanModuleConfig configures the CAN pressure input module's protocol
// (request/response frame IDs, function code, polling) and each of its
// 6 channels. Matches the protocol confirmed against the real module:
// sending <RequestID>#<FunctionCode><00><ChannelCount> (e.g. the default
// below sends exactly 101#020006) gets back ceil(ChannelCount/3)
// response frames on <ResponseID>, each carrying up to 3 big-endian
// 16-bit raw values starting at the frame's channel offset.
type CanModuleConfig struct {
	Interface       string
	RequestID       uint32
	ResponseID      uint32
	FunctionCode    byte
	ChannelCount    int
	PollPeriod      time.Duration
	ResponseTimeout time.Duration
	// LiveEnabled selects the data source for every assigned channel:
	// true reads the real CAN bus; false serves editable simulated
	// values instead (see PressureModule in pressuremodule.go). Starts
	// false so the module doesn't touch the bus until an operator has
	// confirmed wiring and calibration from the admin screen.
	LiveEnabled bool
	Channels    [CanChannelCount]CanChannelConfig
}

// TempChannelConfig maps one physical DS18B20 1-Wire sensor to a logical
// temperature reading. Unlike the CAN pressure module's channels, these
// aren't physically numbered - a device's slot is just "the Nth assigned
// temperature reading" - so DeviceID (its 1-Wire ROM ID, e.g.
// "28-0000123456ef", as listed under /sys/bus/w1/devices) is what
// actually ties it to hardware. DS18B20s report already-calibrated °C,
// so unlike the CAN pressure channels there's no linear fit to configure.
type TempChannelConfig struct {
	Enabled  bool
	Assigned SensorInput // meaningful only when Enabled; must be a temperature sensor
	DeviceID string      // 1-Wire ROM ID, e.g. "28-0000123456ef"; empty until wired up
}

// TempChannelCount matches the number of temperature readings the system
// uses (see allTempSensors in controller.go) - one DS18B20 slot per
// logical reading.
const TempChannelCount = 2

// TempModuleConfig configures the DS18B20 1-Wire temperature sensors.
// The temperatures this feeds change slowly (minutes, not milliseconds),
// so the default poll period is much slower than the CAN pressure
// module's.
type TempModuleConfig struct {
	PollPeriod time.Duration
	// LiveEnabled selects the data source for every assigned channel,
	// mirroring CanModuleConfig.LiveEnabled: true reads /sys/bus/w1/devices;
	// false serves editable simulated values instead. Starts false so
	// the module doesn't depend on any 1-Wire wiring being present until
	// an operator has confirmed it from the admin screen.
	LiveEnabled bool
	Channels    [TempChannelCount]TempChannelConfig
}

// RelayChannelConfig maps one physical relay output on the MCP23017 GPIO
// expander to a logical valve. Port is the expander's own pin number
// (0-15, as it appears via the kernel gpiochip character device) - not a
// Broadcom SoC GPIO number.
type RelayChannelConfig struct {
	Enabled  bool
	Assigned RelayOutput // meaningful only when Enabled
	Port     int         // 0-15, the MCP23017 pin driving this relay
}

// RelayChannelCount matches the number of valves/pumps the system drives
// (see allRelays in controller.go) - one MCP23017 port slot per output.
const RelayChannelCount = 7

// RelayModuleConfig configures the MCP23017 relay outputs, addressed via
// the kernel gpiochip character device (e.g. /dev/gpiochip16).
type RelayModuleConfig struct {
	Chip string // gpiochip device path, e.g. "/dev/gpiochip16"
	// LiveEnabled selects the destination for every assigned channel,
	// mirroring CanModuleConfig.LiveEnabled: true drives the real GPIO
	// expander; false leaves relay state in the in-memory simulator
	// only. Starts false so nothing touches real hardware until an
	// operator has confirmed port wiring from the admin screen.
	LiveEnabled bool
	Channels    [RelayChannelCount]RelayChannelConfig
}

// DigitalInputChannelConfig maps one physical digital input on the SoC's
// own GPIO controller to a logical feedback signal. Port is restricted
// to validDigitalInputPorts (see liveconfig.go) - a hardware constraint
// on this board, unlike the relay outputs' free 0-15 range.
type DigitalInputChannelConfig struct {
	Enabled  bool
	Assigned DigitalInput // meaningful only when Enabled
	Port     int
}

// DigitalInputChannelCount matches the number of digital feedback
// signals the system reads (see allDigitalInputs in controller.go).
const DigitalInputChannelCount = 3

// DigitalInputModuleConfig configures the digital feedback inputs
// (hydraulic oil level, motor-powered confirmation), addressed via the
// kernel gpiochip character device on a different chip than the relay
// outputs (e.g. /dev/gpiochip0 vs. the relays' /dev/gpiochip16).
type DigitalInputModuleConfig struct {
	Chip string // gpiochip device path, e.g. "/dev/gpiochip0"
	// LiveEnabled selects the data source for every assigned channel:
	// true reads the real GPIO line; false serves an editable simulated
	// value instead. Starts false so nothing is read from real hardware
	// until an operator has confirmed port wiring from the admin screen.
	LiveEnabled bool
	Channels    [DigitalInputChannelCount]DigitalInputChannelConfig
}

// TankConfig describes the low-pressure storage bank feeding cylinder A,
// used only for the delivered-mass estimate (see gas.go).
type TankConfig struct {
	VolumeLitres       float64
	NominalPressureBar float64
	NominalTempC       float64
}

type VentConfig struct {
	Duration time.Duration // how long the vent valve stays open to bleed the 700 bar hose
}

type Config struct {
	ListenAddr string
	// SystemName is shown as the title in the banner at the top of
	// every web page - purely cosmetic (site branding), editable from
	// the admin screen like any other tunable.
	SystemName string
	// Language selects the web UI's display language for every page -
	// one of the codes in ValidLanguages (io.go). Editable from the
	// admin screen; does not affect anything on the backend besides
	// this one setting being served back to the pages.
	Language           string
	PollPeriod         time.Duration
	Stage1             StageConfig
	Stage2             StageConfig
	Homing             HomingConfig
	Safety             SafetyConfig
	Tank               TankConfig
	Vent               VentConfig
	CanModule          CanModuleConfig
	TempModule         TempModuleConfig
	RelayModule        RelayModuleConfig
	DigitalInputModule DigitalInputModuleConfig
}

// DefaultConfig gives sane starting values for commissioning against the
// simulated IO. Every value here should be reviewed and re-tuned against
// the real machine (stroke times, spike thresholds and safety limits are
// all machine-specific).
func DefaultConfig() Config {
	return Config{
		ListenAddr: ":8080",
		SystemName: "Hydrogen Booster",
		Language:   "en",
		PollPeriod: 100 * time.Millisecond,
		Stage1: StageConfig{
			Name: "Stage 1 (A/B)", DirRelay: Valve1A, ReliefRelay: Valve1B,
			HydPressureSensor: Hyd1Pressure,
			MinStrokeTime:     300 * time.Millisecond,
			MaxStrokeTime:     6 * time.Second,
			SpikeDeltaBar:     15,
			SpikeWindow:       250 * time.Millisecond,
			ReverseDelay:      500 * time.Millisecond,
		},
		Stage2: StageConfig{
			Name: "Stage 2 (C/D)", DirRelay: Valve2A, ReliefRelay: Valve2B,
			HydPressureSensor: Hyd2Pressure,
			MinStrokeTime:     300 * time.Millisecond,
			MaxStrokeTime:     6 * time.Second,
			SpikeDeltaBar:     15,
			SpikeWindow:       250 * time.Millisecond,
			ReverseDelay:      500 * time.Millisecond,
		},
		Homing: HomingConfig{Duration: 8 * time.Second},
		Safety: SafetyConfig{
			MaxHydraulicPressureBar: 250,
			MaxHydrogenPressureBar:  750,
			MaxTempC:                85,
			SensorMaxAge:            2 * time.Second,
		},
		Tank: TankConfig{VolumeLitres: 300, NominalPressureBar: 200, NominalTempC: 15},
		Vent: VentConfig{Duration: 5 * time.Second},
		// CAN pressure input module: 6 channels, protocol confirmed
		// against the real hardware (101#020006 -> two 8-byte response
		// frames on ID 0x001, each carrying 3 big-endian 16-bit raw
		// values). Calibration below is a PLACEHOLDER (0-65535 raw
		// mapped straight across an assumed full-scale bar figure) -
		// every channel must be calibrated against known applied
		// pressures before the readings mean anything. Starts in
		// simulation mode (LiveEnabled: false) until that's done.
		CanModule: CanModuleConfig{
			Interface:       "can0",
			RequestID:       0x101,
			ResponseID:      0x001,
			FunctionCode:    0x02,
			ChannelCount:    CanChannelCount,
			PollPeriod:      200 * time.Millisecond,
			ResponseTimeout: 300 * time.Millisecond,
			LiveEnabled:     false,
			Channels: [CanChannelCount]CanChannelConfig{
				{Enabled: true, Assigned: Hyd1Pressure, CalLowRaw: 0, CalLowBar: 0, CalHighRaw: 65535, CalHighBar: 250},
				{Enabled: true, Assigned: Hyd2Pressure, CalLowRaw: 0, CalLowBar: 0, CalHighRaw: 65535, CalHighBar: 250},
				{Enabled: true, Assigned: TankPressure, CalLowRaw: 0, CalLowBar: 0, CalHighRaw: 65535, CalHighBar: 300},
				{Enabled: true, Assigned: InterStagePressure, CalLowRaw: 0, CalLowBar: 0, CalHighRaw: 65535, CalHighBar: 500},
				{Enabled: true, Assigned: OutputPressure, CalLowRaw: 0, CalLowBar: 0, CalHighRaw: 65535, CalHighBar: 750},
				{Enabled: false}, // spare channel
			},
		},
		// DS18B20 1-Wire temperature sensors: one slot per logical
		// temperature reading. DeviceID starts empty - pick each
		// physical sensor's ROM ID from the admin screen once it's
		// wired up and discoverable under /sys/bus/w1/devices. Starts
		// in simulation mode (LiveEnabled: false) like the CAN module.
		TempModule: TempModuleConfig{
			PollPeriod:  5 * time.Second,
			LiveEnabled: false,
			Channels: [TempChannelCount]TempChannelConfig{
				{Enabled: true, Assigned: HydOilTemp},
				{Enabled: true, Assigned: OutputTemp},
			},
		},
		// MCP23017 relay outputs: one port slot per valve. Ports below
		// are placeholders (0-4, in valve declaration order) - confirm
		// each against the real wiring before enabling live data.
		RelayModule: RelayModuleConfig{
			Chip:        "/dev/gpiochip16",
			LiveEnabled: false,
			Channels: [RelayChannelCount]RelayChannelConfig{
				{Enabled: true, Assigned: Valve1A, Port: 0},
				{Enabled: true, Assigned: Valve1B, Port: 1},
				{Enabled: true, Assigned: Valve2A, Port: 2},
				{Enabled: true, Assigned: Valve2B, Port: 3},
				{Enabled: true, Assigned: VentValve, Port: 4},
				{Enabled: true, Assigned: Pump1, Port: 5},
				{Enabled: true, Assigned: Pump2, Port: 6},
			},
		},
		// Digital feedback inputs on the SoC's own GPIO controller
		// (gpiochip0) - a different chip than the relay outputs above.
		// Port choices below are placeholders from the allowed set
		// (validDigitalInputPorts, liveconfig.go); confirm each against
		// the real wiring before enabling live data.
		DigitalInputModule: DigitalInputModuleConfig{
			Chip:        "/dev/gpiochip0",
			LiveEnabled: false,
			Channels: [DigitalInputChannelCount]DigitalInputChannelConfig{
				{Enabled: true, Assigned: HydraulicOilLevel, Port: 2},
				{Enabled: true, Assigned: Motor1Powered, Port: 3},
				{Enabled: true, Assigned: Motor2Powered, Port: 5},
			},
		},
	}
}
