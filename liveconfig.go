package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LiveConfig holds the operating configuration behind a read/write mutex
// so it can be safely read on every control-loop tick and updated at any
// time - e.g. from the admin web interface - without restarting the
// program. Config contains only value types (no slices/maps/pointers),
// so Snapshot() returning a copy is cheap and safe to call as often as
// needed.
type LiveConfig struct {
	mu  sync.RWMutex
	cfg Config
}

func NewLiveConfig(initial Config) *LiveConfig {
	return &LiveConfig{cfg: initial}
}

func (l *LiveConfig) Snapshot() Config {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.cfg
}

func (l *LiveConfig) Set(c Config) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg = c
}

// ---------------------------------------------------------------------
// Web-editable configuration DTO
//
// Only timing and pressure/temperature limits are exposed here for
// on-the-fly editing from the admin screen. Relay and sensor channel
// *assignments* (which physical output/input each field refers to) are
// wiring, not tuning, and are deliberately left out - change those in
// config.go and restart if the physical wiring changes.
// ---------------------------------------------------------------------

type StageConfigDTO struct {
	Name            string  `json:"name"`
	MinStrokeTimeMs int     `json:"minStrokeTimeMs"`
	MaxStrokeTimeMs int     `json:"maxStrokeTimeMs"`
	SpikeDeltaBar   float64 `json:"spikeDeltaBar"`
	SpikeWindowMs   int     `json:"spikeWindowMs"`
	PulseDurationMs int     `json:"pulseDurationMs"`
}

type SafetyConfigDTO struct {
	MaxHydraulicPressureBar float64 `json:"maxHydraulicPressureBar"`
	MaxHydrogenPressureBar  float64 `json:"maxHydrogenPressureBar"`
	MaxTempC                float64 `json:"maxTempC"`
	SensorMaxAgeMs          int     `json:"sensorMaxAgeMs"`
	MaxSyncStrokeLead       int     `json:"maxSyncStrokeLead"`
}

type TankConfigDTO struct {
	VolumeLitres       float64 `json:"volumeLitres"`
	NominalPressureBar float64 `json:"nominalPressureBar"`
	NominalTempC       float64 `json:"nominalTempC"`
}

type VentConfigDTO struct {
	DurationMs int `json:"durationMs"`
}

type CanChannelConfigDTO struct {
	Enabled    bool    `json:"enabled"`
	Assigned   string  `json:"assigned,omitempty"` // sensor name (see io.go SensorInput.String); empty when not Enabled
	CalLowRaw  int     `json:"calLowRaw"`
	CalLowBar  float64 `json:"calLowBar"`
	CalHighRaw int     `json:"calHighRaw"`
	CalHighBar float64 `json:"calHighBar"`
}

type CanModuleConfigDTO struct {
	Interface         string                `json:"interface"`
	RequestID         string                `json:"requestId"`  // hex, e.g. "0x101"
	ResponseID        string                `json:"responseId"` // hex, e.g. "0x001"
	FunctionCode      int                   `json:"functionCode"`
	PollPeriodMs      int                   `json:"pollPeriodMs"`
	ResponseTimeoutMs int                   `json:"responseTimeoutMs"`
	LiveEnabled       bool                  `json:"liveEnabled"`
	Channels          []CanChannelConfigDTO `json:"channels"`
}

type TempChannelConfigDTO struct {
	Enabled  bool   `json:"enabled"`
	Assigned string `json:"assigned,omitempty"` // sensor name (see io.go SensorInput.String); empty when not Enabled
	DeviceID string `json:"deviceId,omitempty"` // 1-Wire ROM ID, e.g. "28-0000123456ef"
}

type TempModuleConfigDTO struct {
	PollPeriodMs int                    `json:"pollPeriodMs"`
	LiveEnabled  bool                   `json:"liveEnabled"`
	Channels     []TempChannelConfigDTO `json:"channels"`
}

type RelayChannelConfigDTO struct {
	Enabled  bool   `json:"enabled"`
	Assigned string `json:"assigned,omitempty"` // valve name (see io.go RelayOutput.String); empty when not Enabled
	Port     int    `json:"port"`
}

type RelayModuleConfigDTO struct {
	Chip        string                  `json:"chip"`
	LiveEnabled bool                    `json:"liveEnabled"`
	Channels    []RelayChannelConfigDTO `json:"channels"`
}

type DigitalInputChannelConfigDTO struct {
	Enabled  bool   `json:"enabled"`
	Assigned string `json:"assigned,omitempty"` // signal name (see io.go DigitalInput.String); empty when not Enabled
	Port     int    `json:"port"`
}

type DigitalInputModuleConfigDTO struct {
	Chip        string                         `json:"chip"`
	LiveEnabled bool                           `json:"liveEnabled"`
	Channels    []DigitalInputChannelConfigDTO `json:"channels"`
}

type ConfigDTO struct {
	SystemName         string                      `json:"systemName"`
	Language           string                      `json:"language"`
	PollPeriodMs       int                         `json:"pollPeriodMs"`
	Stage1             StageConfigDTO              `json:"stage1"`
	Stage2             StageConfigDTO              `json:"stage2"`
	Safety             SafetyConfigDTO             `json:"safety"`
	Tank               TankConfigDTO               `json:"tank"`
	Vent               VentConfigDTO               `json:"vent"`
	CanModule          CanModuleConfigDTO          `json:"canModule"`
	TempModule         TempModuleConfigDTO         `json:"tempModule"`
	RelayModule        RelayModuleConfigDTO        `json:"relayModule"`
	DigitalInputModule DigitalInputModuleConfigDTO `json:"digitalInputModule"`
}

func stageToDTO(s StageConfig) StageConfigDTO {
	return StageConfigDTO{
		Name:            s.Name,
		MinStrokeTimeMs: int(s.MinStrokeTime / time.Millisecond),
		MaxStrokeTimeMs: int(s.MaxStrokeTime / time.Millisecond),
		SpikeDeltaBar:   s.SpikeDeltaBar,
		SpikeWindowMs:   int(s.SpikeWindow / time.Millisecond),
		PulseDurationMs: int(s.PulseDuration / time.Millisecond),
	}
}

// formatCanID/parseCanID round-trip a CAN arbitration ID through a hex
// string ("0x101") in the JSON config file and admin screen, matching
// how they're written with cansend/candump.
func formatCanID(id uint32) string { return fmt.Sprintf("0x%X", id) }

func parseCanID(s string) (uint32, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 0, 32) // base 0: accepts "0x101" or "257"
	if err != nil {
		return 0, fmt.Errorf("invalid CAN ID %q: %w", s, err)
	}
	return uint32(v), nil
}

func canModuleToDTO(c CanModuleConfig) CanModuleConfigDTO {
	dto := CanModuleConfigDTO{
		Interface:         c.Interface,
		RequestID:         formatCanID(c.RequestID),
		ResponseID:        formatCanID(c.ResponseID),
		FunctionCode:      int(c.FunctionCode),
		PollPeriodMs:      int(c.PollPeriod / time.Millisecond),
		ResponseTimeoutMs: int(c.ResponseTimeout / time.Millisecond),
		LiveEnabled:       c.LiveEnabled,
	}
	for _, ch := range c.Channels {
		assigned := ""
		if ch.Enabled {
			assigned = ch.Assigned.String()
		}
		dto.Channels = append(dto.Channels, CanChannelConfigDTO{
			Enabled: ch.Enabled, Assigned: assigned,
			CalLowRaw: ch.CalLowRaw, CalLowBar: ch.CalLowBar,
			CalHighRaw: ch.CalHighRaw, CalHighBar: ch.CalHighBar,
		})
	}
	return dto
}

func tempModuleToDTO(c TempModuleConfig) TempModuleConfigDTO {
	dto := TempModuleConfigDTO{
		PollPeriodMs: int(c.PollPeriod / time.Millisecond),
		LiveEnabled:  c.LiveEnabled,
	}
	for _, ch := range c.Channels {
		assigned := ""
		if ch.Enabled {
			assigned = ch.Assigned.String()
		}
		dto.Channels = append(dto.Channels, TempChannelConfigDTO{
			Enabled: ch.Enabled, Assigned: assigned, DeviceID: ch.DeviceID,
		})
	}
	return dto
}

func relayModuleToDTO(c RelayModuleConfig) RelayModuleConfigDTO {
	dto := RelayModuleConfigDTO{Chip: c.Chip, LiveEnabled: c.LiveEnabled}
	for _, ch := range c.Channels {
		assigned := ""
		if ch.Enabled {
			assigned = ch.Assigned.String()
		}
		dto.Channels = append(dto.Channels, RelayChannelConfigDTO{
			Enabled: ch.Enabled, Assigned: assigned, Port: ch.Port,
		})
	}
	return dto
}

func digitalInputModuleToDTO(c DigitalInputModuleConfig) DigitalInputModuleConfigDTO {
	dto := DigitalInputModuleConfigDTO{Chip: c.Chip, LiveEnabled: c.LiveEnabled}
	for _, ch := range c.Channels {
		assigned := ""
		if ch.Enabled {
			assigned = ch.Assigned.String()
		}
		dto.Channels = append(dto.Channels, DigitalInputChannelConfigDTO{
			Enabled: ch.Enabled, Assigned: assigned, Port: ch.Port,
		})
	}
	return dto
}

// ToDTO converts the internal Config into the editable shape the admin
// screen works with.
func ToDTO(c Config) ConfigDTO {
	return ConfigDTO{
		SystemName:   c.SystemName,
		Language:     c.Language,
		PollPeriodMs: int(c.PollPeriod / time.Millisecond),
		Stage1:       stageToDTO(c.Stage1),
		Stage2:       stageToDTO(c.Stage2),
		Safety: SafetyConfigDTO{
			MaxHydraulicPressureBar: c.Safety.MaxHydraulicPressureBar,
			MaxHydrogenPressureBar:  c.Safety.MaxHydrogenPressureBar,
			MaxTempC:                c.Safety.MaxTempC,
			SensorMaxAgeMs:          int(c.Safety.SensorMaxAge / time.Millisecond),
			MaxSyncStrokeLead:       c.Safety.MaxSyncStrokeLead,
		},
		Tank: TankConfigDTO{
			VolumeLitres:       c.Tank.VolumeLitres,
			NominalPressureBar: c.Tank.NominalPressureBar,
			NominalTempC:       c.Tank.NominalTempC,
		},
		Vent:               VentConfigDTO{DurationMs: int(c.Vent.Duration / time.Millisecond)},
		CanModule:          canModuleToDTO(c.CanModule),
		TempModule:         tempModuleToDTO(c.TempModule),
		RelayModule:        relayModuleToDTO(c.RelayModule),
		DigitalInputModule: digitalInputModuleToDTO(c.DigitalInputModule),
	}
}

// applyStage merges an editable DTO onto an existing StageConfig,
// preserving the existing wiring (relay/sensor assignments, name).
func applyStage(base StageConfig, dto StageConfigDTO) StageConfig {
	base.MinStrokeTime = time.Duration(dto.MinStrokeTimeMs) * time.Millisecond
	base.MaxStrokeTime = time.Duration(dto.MaxStrokeTimeMs) * time.Millisecond
	base.SpikeDeltaBar = dto.SpikeDeltaBar
	base.SpikeWindow = time.Duration(dto.SpikeWindowMs) * time.Millisecond
	base.PulseDuration = time.Duration(dto.PulseDurationMs) * time.Millisecond
	return base
}

func validateStageDTO(label string, s StageConfigDTO) error {
	if s.MinStrokeTimeMs < 0 {
		return fmt.Errorf("%s: min stroke time cannot be negative", label)
	}
	if s.MaxStrokeTimeMs <= s.MinStrokeTimeMs {
		return fmt.Errorf("%s: max stroke time must be greater than min stroke time", label)
	}
	if s.SpikeDeltaBar <= 0 {
		return fmt.Errorf("%s: spike delta must be greater than zero", label)
	}
	if s.SpikeWindowMs <= 0 {
		return fmt.Errorf("%s: spike window must be greater than zero", label)
	}
	if s.PulseDurationMs <= 0 {
		return fmt.Errorf("%s: pulse duration must be greater than zero", label)
	}
	return nil
}

// FromDTO merges an editable ConfigDTO onto an existing Config (keeping
// wiring/listen-address fields from base) and validates it. It does not
// mutate base; call LiveConfig.Set with the result.
func FromDTO(base Config, dto ConfigDTO) (Config, error) {
	if dto.PollPeriodMs < 10 {
		return base, fmt.Errorf("poll period must be at least 10ms")
	}
	if err := validateStageDTO("stage1", dto.Stage1); err != nil {
		return base, err
	}
	if err := validateStageDTO("stage2", dto.Stage2); err != nil {
		return base, err
	}
	if dto.Safety.MaxHydraulicPressureBar <= 0 || dto.Safety.MaxHydrogenPressureBar <= 0 || dto.Safety.MaxTempC <= 0 {
		return base, fmt.Errorf("safety limits must be greater than zero")
	}
	if dto.Safety.SensorMaxAgeMs < 0 {
		return base, fmt.Errorf("sensor max age cannot be negative")
	}
	if dto.Safety.MaxSyncStrokeLead < 1 {
		return base, fmt.Errorf("max sync stroke lead must be at least 1")
	}
	if dto.Tank.VolumeLitres <= 0 {
		return base, fmt.Errorf("tank volume must be greater than zero")
	}
	if dto.Vent.DurationMs < 0 {
		return base, fmt.Errorf("vent duration cannot be negative")
	}

	out := base
	// A blank name is treated as "no change" rather than wiped to empty -
	// this is a cosmetic field, not something that should go missing from
	// a config file that predates it, or from a request that just forgot
	// to include it.
	if name := strings.TrimSpace(dto.SystemName); name != "" {
		out.SystemName = name
	}
	if lang := strings.TrimSpace(dto.Language); lang != "" {
		if !ValidLanguages[lang] {
			return base, fmt.Errorf("unsupported language %q", lang)
		}
		out.Language = lang
	}
	out.PollPeriod = time.Duration(dto.PollPeriodMs) * time.Millisecond
	out.Stage1 = applyStage(base.Stage1, dto.Stage1)
	out.Stage2 = applyStage(base.Stage2, dto.Stage2)
	out.Safety.MaxHydraulicPressureBar = dto.Safety.MaxHydraulicPressureBar
	out.Safety.MaxHydrogenPressureBar = dto.Safety.MaxHydrogenPressureBar
	out.Safety.MaxTempC = dto.Safety.MaxTempC
	out.Safety.SensorMaxAge = time.Duration(dto.Safety.SensorMaxAgeMs) * time.Millisecond
	out.Safety.MaxSyncStrokeLead = dto.Safety.MaxSyncStrokeLead
	out.Tank.VolumeLitres = dto.Tank.VolumeLitres
	out.Tank.NominalPressureBar = dto.Tank.NominalPressureBar
	out.Tank.NominalTempC = dto.Tank.NominalTempC
	out.Vent.Duration = time.Duration(dto.Vent.DurationMs) * time.Millisecond

	canCfg, err := canModuleFromDTO(base.CanModule, dto.CanModule)
	if err != nil {
		return base, err
	}
	out.CanModule = canCfg

	tempCfg, err := tempModuleFromDTO(base.TempModule, dto.TempModule)
	if err != nil {
		return base, err
	}
	out.TempModule = tempCfg

	relayCfg, err := relayModuleFromDTO(base.RelayModule, dto.RelayModule)
	if err != nil {
		return base, err
	}
	out.RelayModule = relayCfg

	digitalInputCfg, err := digitalInputModuleFromDTO(base.DigitalInputModule, dto.DigitalInputModule)
	if err != nil {
		return base, err
	}
	out.DigitalInputModule = digitalInputCfg

	return out, nil
}

// canModuleFromDTO validates and merges the CAN pressure module's DTO
// onto the existing config. Kept separate from FromDTO above for
// readability, given how much channel-level validation there is.
func canModuleFromDTO(base CanModuleConfig, dto CanModuleConfigDTO) (CanModuleConfig, error) {
	if dto.Interface == "" {
		return base, fmt.Errorf("CAN interface name cannot be empty")
	}
	reqID, err := parseCanID(dto.RequestID)
	if err != nil {
		return base, err
	}
	respID, err := parseCanID(dto.ResponseID)
	if err != nil {
		return base, err
	}
	if dto.FunctionCode < 0 || dto.FunctionCode > 255 {
		return base, fmt.Errorf("CAN function code must be 0-255")
	}
	if dto.PollPeriodMs < 10 {
		return base, fmt.Errorf("CAN poll period must be at least 10ms")
	}
	if dto.ResponseTimeoutMs < 10 {
		return base, fmt.Errorf("CAN response timeout must be at least 10ms")
	}
	if len(dto.Channels) != CanChannelCount {
		return base, fmt.Errorf("CAN module must have exactly %d channels", CanChannelCount)
	}

	seen := map[SensorInput]bool{}
	var channels [CanChannelCount]CanChannelConfig
	for i, chDto := range dto.Channels {
		ch := CanChannelConfig{
			Enabled:    chDto.Enabled,
			CalLowRaw:  chDto.CalLowRaw,
			CalLowBar:  chDto.CalLowBar,
			CalHighRaw: chDto.CalHighRaw,
			CalHighBar: chDto.CalHighBar,
		}
		if chDto.Enabled {
			s, ok := ParseSensorInput(chDto.Assigned)
			if !ok || !isPressureSensor(s) {
				return base, fmt.Errorf("CAN channel %d: %q is not a valid pressure sensor assignment", i, chDto.Assigned)
			}
			if seen[s] {
				return base, fmt.Errorf("CAN channel %d: %s is already assigned to another enabled channel", i, s.String())
			}
			seen[s] = true
			ch.Assigned = s
			if chDto.CalLowRaw < 0 || chDto.CalLowRaw > 65535 || chDto.CalHighRaw < 0 || chDto.CalHighRaw > 65535 {
				return base, fmt.Errorf("CAN channel %d: calibration raw values must be 0-65535", i)
			}
			if chDto.CalHighRaw == chDto.CalLowRaw {
				return base, fmt.Errorf("CAN channel %d: high and low calibration raw values must differ", i)
			}
		}
		channels[i] = ch
	}

	return CanModuleConfig{
		Interface:       dto.Interface,
		RequestID:       reqID,
		ResponseID:      respID,
		FunctionCode:    byte(dto.FunctionCode),
		ChannelCount:    CanChannelCount,
		PollPeriod:      time.Duration(dto.PollPeriodMs) * time.Millisecond,
		ResponseTimeout: time.Duration(dto.ResponseTimeoutMs) * time.Millisecond,
		LiveEnabled:     dto.LiveEnabled,
		Channels:        channels,
	}, nil
}

// tempModuleFromDTO validates and merges the 1-Wire temperature module's
// DTO onto the existing config. A channel may be enabled with no
// DeviceID yet (not wired up) - it just won't produce readings until one
// is selected on the admin screen.
func tempModuleFromDTO(base TempModuleConfig, dto TempModuleConfigDTO) (TempModuleConfig, error) {
	if dto.PollPeriodMs < 1000 {
		return base, fmt.Errorf("temperature poll period must be at least 1000ms")
	}
	if len(dto.Channels) != TempChannelCount {
		return base, fmt.Errorf("temperature module must have exactly %d channels", TempChannelCount)
	}

	seen := map[SensorInput]bool{}
	var channels [TempChannelCount]TempChannelConfig
	for i, chDto := range dto.Channels {
		ch := TempChannelConfig{Enabled: chDto.Enabled, DeviceID: strings.TrimSpace(chDto.DeviceID)}
		if chDto.Enabled {
			s, ok := ParseSensorInput(chDto.Assigned)
			if !ok || !isTempSensor(s) {
				return base, fmt.Errorf("temperature channel %d: %q is not a valid temperature sensor assignment", i, chDto.Assigned)
			}
			if seen[s] {
				return base, fmt.Errorf("temperature channel %d: %s is already assigned to another enabled channel", i, s.String())
			}
			seen[s] = true
			ch.Assigned = s
		}
		channels[i] = ch
	}

	return TempModuleConfig{
		PollPeriod:  time.Duration(dto.PollPeriodMs) * time.Millisecond,
		LiveEnabled: dto.LiveEnabled,
		Channels:    channels,
	}, nil
}

// relayModuleFromDTO validates and merges the MCP23017 relay module's
// DTO onto the existing config. Rejects two enabled channels sharing a
// port or a valve up front - a wiring mistake here means the wrong
// physical valve fires, so it's worth catching before Apply rather than
// discovering it during a fill.
func relayModuleFromDTO(base RelayModuleConfig, dto RelayModuleConfigDTO) (RelayModuleConfig, error) {
	if strings.TrimSpace(dto.Chip) == "" {
		return base, fmt.Errorf("GPIO relay chip path cannot be empty")
	}
	if len(dto.Channels) != RelayChannelCount {
		return base, fmt.Errorf("relay module must have exactly %d channels", RelayChannelCount)
	}

	seenValve := map[RelayOutput]bool{}
	seenPort := map[int]bool{}
	var channels [RelayChannelCount]RelayChannelConfig
	for i, chDto := range dto.Channels {
		ch := RelayChannelConfig{Enabled: chDto.Enabled, Port: chDto.Port}
		if chDto.Enabled {
			r, ok := ParseRelayOutput(chDto.Assigned)
			if !ok {
				return base, fmt.Errorf("relay channel %d: %q is not a valid valve assignment", i, chDto.Assigned)
			}
			if seenValve[r] {
				return base, fmt.Errorf("relay channel %d: %s is already assigned to another enabled channel", i, r.String())
			}
			seenValve[r] = true
			if chDto.Port < 0 || chDto.Port > 15 {
				return base, fmt.Errorf("relay channel %d: port must be 0-15", i)
			}
			if seenPort[chDto.Port] {
				return base, fmt.Errorf("relay channel %d: port %d is already used by another enabled channel", i, chDto.Port)
			}
			seenPort[chDto.Port] = true
			ch.Assigned = r
		}
		channels[i] = ch
	}

	return RelayModuleConfig{
		Chip:        strings.TrimSpace(dto.Chip),
		LiveEnabled: dto.LiveEnabled,
		Channels:    channels,
	}, nil
}

// validDigitalInputPorts is the hardware-constrained set of gpiochip0
// line numbers these digital inputs may be wired to - unlike the relay
// outputs' free 0-15 range, only these lines are usable on this board.
var validDigitalInputPorts = map[int]bool{2: true, 3: true, 5: true, 6: true, 7: true, 12: true, 13: true}

// digitalInputModuleFromDTO validates and merges the digital input
// module's DTO onto the existing config, mirroring relayModuleFromDTO's
// shape - duplicate assignment/port checks, plus the allowed-line-number
// restriction relay ports don't have.
func digitalInputModuleFromDTO(base DigitalInputModuleConfig, dto DigitalInputModuleConfigDTO) (DigitalInputModuleConfig, error) {
	if strings.TrimSpace(dto.Chip) == "" {
		return base, fmt.Errorf("GPIO digital input chip path cannot be empty")
	}
	if len(dto.Channels) != DigitalInputChannelCount {
		return base, fmt.Errorf("digital input module must have exactly %d channels", DigitalInputChannelCount)
	}

	seenSignal := map[DigitalInput]bool{}
	seenPort := map[int]bool{}
	var channels [DigitalInputChannelCount]DigitalInputChannelConfig
	for i, chDto := range dto.Channels {
		ch := DigitalInputChannelConfig{Enabled: chDto.Enabled, Port: chDto.Port}
		if chDto.Enabled {
			d, ok := ParseDigitalInput(chDto.Assigned)
			if !ok {
				return base, fmt.Errorf("digital input channel %d: %q is not a valid signal assignment", i, chDto.Assigned)
			}
			if seenSignal[d] {
				return base, fmt.Errorf("digital input channel %d: %s is already assigned to another enabled channel", i, d.String())
			}
			seenSignal[d] = true
			if !validDigitalInputPorts[chDto.Port] {
				return base, fmt.Errorf("digital input channel %d: port %d is not one of the allowed lines (2,3,5,6,7,12,13)", i, chDto.Port)
			}
			if seenPort[chDto.Port] {
				return base, fmt.Errorf("digital input channel %d: port %d is already used by another enabled channel", i, chDto.Port)
			}
			seenPort[chDto.Port] = true
			ch.Assigned = d
		}
		channels[i] = ch
	}

	return DigitalInputModuleConfig{
		Chip:        strings.TrimSpace(dto.Chip),
		LiveEnabled: dto.LiveEnabled,
		Channels:    channels,
	}, nil
}
