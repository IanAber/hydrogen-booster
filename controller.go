package main

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

type NozzleSize int

const (
	Nozzle350 NozzleSize = 350
	Nozzle700 NozzleSize = 700
)

type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseFilling  Phase = "filling"
	PhaseVenting  Phase = "venting"
	PhaseComplete Phase = "complete"
	PhaseFault    Phase = "fault"
)

type JobRequest struct {
	TargetPressureBar float64
	TargetMassKg      float64
	Nozzle            NozzleSize
	// CustomerCode is the customer's login code, entered on the public
	// customer screen before Start is enabled there. 1-10 digits. Start
	// checks it against the codes list and resolves it to the customer
	// (ID and name) - the code itself goes no further: it's never
	// logged, kept with the job, or recorded with the dispense.
	CustomerCode string
}

type Status struct {
	Phase             Phase       `json:"phase"`
	Running           bool        `json:"running"`
	Nozzle            int         `json:"nozzle"`
	TargetPressureBar float64     `json:"targetPressureBar"`
	TargetMassKg      float64     `json:"targetMassKg"`
	OutputPressureBar float64     `json:"outputPressureBar"`
	OutputTempC       float64     `json:"outputTempC"`
	TankPressureBar   float64     `json:"tankPressureBar"`
	HydOilTempC       float64     `json:"hydOilTempC"`
	DeliveredMassKg   float64     `json:"deliveredMassKg"`
	Stage1            StageStatus `json:"stage1"`
	Stage2            StageStatus `json:"stage2"`
	VentOpen          bool        `json:"ventOpen"`
	Faults            []string    `json:"faults,omitempty"`
	// FaultDetails carries the same faults as Faults, with each one's
	// code/params so the web UI can show it in the operator's language.
	FaultDetails []FaultRecord `json:"faultDetails,omitempty"`
	StartedAt    *time.Time    `json:"startedAt,omitempty"`
	ElapsedSec   float64       `json:"elapsedSec"`
	ManualMode   bool          `json:"manualMode"`
	Nozzles      NozzleStatus  `json:"nozzles"`
}

// Controller is the top level supervisor: it runs the two Stages, keeps
// them synchronised, enforces safety interlocks, and sequences a fill job
// from Start through target-reached, optional vent, to Complete. All
// tuning comes from LiveConfig, so it can be changed on the fly (e.g.
// from the admin web screen) without restarting.
type Controller struct {
	live *LiveConfig
	io   IO

	stages *Stages

	mu            sync.Mutex
	phase         Phase
	job           JobRequest
	startedAt     time.Time
	initialTankKg float64
	faults        []FaultRecord
	faultKeys     map[string]bool // see raiseFaultKey; reset whenever faults is
	// sensorProblems is superviseSafety's own record (key -> text) of
	// the sensor problems seen on its last pass - only ever touched from
	// the control loop - used to log when each one clears.
	sensorProblems map[string]string
	// finalDeliveredMassKg is the delivered-mass snapshot taken at the
	// moment a job stops (reaches target, is aborted, or faults).
	// Status() reports this instead of a fresh deliveredMassKg() call
	// once the job isn't actively running - otherwise the figure would
	// keep drifting from ordinary sensor noise (temperature jitter, on
	// real hardware too) even though no gas is moving anymore.
	finalDeliveredMassKg float64

	// ventDemandOffSince/oilAboveSetpoint are tickFans' own persistent
	// state - zero ventDemandOffSince means VentFan is currently demanded
	// (a pump on or hydrogen detected), or the demand has never been
	// observed to clear; oilAboveSetpoint is the temperature hysteresis
	// state, needed because its off-transition depends on that history,
	// not just the instantaneous temperature reading.
	ventDemandOffSince time.Time
	oilAboveSetpoint   bool

	// outputPressureFilterBuf/filteredOutputPressureBar/
	// filteredOutputPressureErr are tickOutputPressureFilter's own
	// persistent state - refreshed once per control-loop tick so every
	// consumer (superviseJob, Status) sees the same filtered reading
	// regardless of how often each is separately invoked (e.g. an HTTP
	// status poll landing between ticks) - see tickOutputPressureFilter.
	outputPressureFilterBuf   []float64
	filteredOutputPressureBar float64
	filteredOutputPressureErr error

	// dispenses and validCodes are optional (nil-safe) - set via
	// SetDispenseStore/SetValidCodes if the caller wants completed fills
	// recorded and customer codes checked against an allow-list. Kept
	// optional (rather than required constructor args) so existing
	// callers/tests that don't care about either keep working unchanged.
	dispenses  *DispenseStore
	validCodes *ValidCodes

	// customer is who the current/last job is for, resolved from its
	// login code by Start (zero if none was entered).
	customer Customer

	// manualMode is set by SetManualMode, from the manual piston control
	// page - see its doc comment.
	manualMode bool

	// manualReverse holds, per stage (index 0 = stage 1, index 1 =
	// stage 2), whether a ManualJog press changed that stage's direction
	// valve and is waiting out ReverseDelay before tickManualJog closes
	// the relief valve - see ManualJog's doc comment.
	manualReverse [2]manualReverseState
}

type manualReverseState struct {
	pending  bool
	deadline time.Time
}

// SetDispenseStore wires up recording of completed fills (see
// superviseJob). Call once before Run(); nil is fine and just means
// completions aren't recorded anywhere.
func (c *Controller) SetDispenseStore(d *DispenseStore) {
	c.dispenses = d
}

// SetValidCodes wires up the admin-maintained customer code allow-list
// (see Start). Call once before serving requests; nil is fine and means
// any well-formed code is accepted, matching the prior behaviour before
// this existed.
func (c *Controller) SetValidCodes(v *ValidCodes) {
	c.validCodes = v
}

func NewController(live *LiveConfig, io IO) *Controller {
	c := &Controller{live: live, io: io, phase: PhaseIdle}
	c.stages = NewStages(live, io)

	// The two circuits always reverse together (see Stages' doc
	// comment for the barrier that guarantees this) - a single log line
	// per synchronized reversal, rather than one per stage.
	c.stages.OnReversal = func(dir Direction, strokeCount uint64) {
		d := "DOWN"
		if dir == DirUp {
			d = "UP"
		}
		log.Printf("Stages reversed -> %s (stroke %d)", d, strokeCount)
	}
	return c
}

// Run is the supervisory loop; call it once in a goroutine for the life
// of the program. The poll period itself is live-tunable: the ticker is
// recreated whenever it changes.
func (c *Controller) Run() {
	period := c.live.Snapshot().PollPeriod
	t := time.NewTicker(period)
	defer t.Stop()
	for now := range t.C {
		c.stages.Tick(now)
		c.superviseSafety()
		c.tickFans(now)
		c.tickManualJog(now)
		c.superviseJob()

		if newPeriod := c.live.Snapshot().PollPeriod; newPeriod != period && newPeriod > 0 {
			t.Stop()
			t = time.NewTicker(newPeriod)
			period = newPeriod
		}
	}
}

// tickOutputPressureFilter refreshes the median-filtered hydrogen
// output pressure reading - called once per control-loop tick, from
// superviseJob (itself called once per tick by Run()) - rejects brief
// spikes/dips caused by the delivery line's non-return (check) valve
// opening and closing. superviseJob's own target-reached detection and
// Status (display/the customer gauge) read the cached result via
// outputPressureBar() rather than re-filtering independently, so the
// filter's window is anchored to the actual poll period regardless of
// how often status is separately requested over HTTP.
//
// Deliberately NOT used by superviseSafety's OutputPressure limit
// check, which reads the sensor directly - see
// Config.OutputPressureFilterSamples' doc comment in config.go for why.
func (c *Controller) tickOutputPressureFilter() {
	raw, err := c.io.ReadPressureBar(OutputPressure)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.filteredOutputPressureErr = err
		return
	}
	n := c.live.Snapshot().OutputPressureFilterSamples
	if n <= 1 {
		c.outputPressureFilterBuf = nil
		c.filteredOutputPressureBar = raw
		c.filteredOutputPressureErr = nil
		return
	}
	c.outputPressureFilterBuf = append(c.outputPressureFilterBuf, raw)
	if len(c.outputPressureFilterBuf) > n {
		c.outputPressureFilterBuf = c.outputPressureFilterBuf[len(c.outputPressureFilterBuf)-n:]
	}
	c.filteredOutputPressureBar = median(c.outputPressureFilterBuf)
	c.filteredOutputPressureErr = nil
}

// outputPressureBar returns the last filtered reading from
// tickOutputPressureFilter.
func (c *Controller) outputPressureBar() (float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.filteredOutputPressureBar, c.filteredOutputPressureErr
}

// sensorProblem is one sensor that's unreadable or outside its limits.
type sensorProblem struct {
	key     string  // stable identity, so a persisting problem is only logged once per fault
	msg     Message // operator-facing description naming the sensor
	pending bool    // a live module hasn't produced its first reading yet - not ready, but not a fault
}

// checkSensors reads every pressure, temperature and digital sensor and
// reports each one that can't be read or (pressure/temperature) reads
// outside its SafetyConfig.Limits range. It reads the raw sensor
// values, deliberately unaffected by OutputPressureFilterSamples, so a
// filtering misconfiguration can never mask a genuine limit breach.
func (c *Controller) checkSensors() []sensorProblem {
	limits := c.live.Snapshot().Safety.Limits
	var out []sensorProblem
	unreadable := func(key, label string, err error) {
		out = append(out, sensorProblem{
			key: "unreadable:" + key,
			msg: msg("sensor_unreadable", map[string]any{"sensor": key, "reason": messageOf(err)},
				"%s sensor unreadable (%v)", label, err),
			pending: errors.Is(err, ErrSensorPending),
		})
	}
	checkValue := func(sn SensorInput, v float64, err error) {
		if err != nil {
			unreadable(sn.String(), sn.Label(), err)
			return
		}
		l := limits[sn]
		params := func(limit float64) map[string]any {
			return map[string]any{"sensor": sn.String(), "value": fmt.Sprintf("%.1f", v), "limit": fmt.Sprintf("%.1f", limit), "unit": sn.Unit()}
		}
		switch {
		case v > l.Max:
			out = append(out, sensorProblem{key: "max:" + sn.String(),
				msg: msg("sensor_above_max", params(l.Max), "%s %.1f %s is above its maximum of %.1f %s", sn.Label(), v, sn.Unit(), l.Max, sn.Unit())})
		case v < l.Min:
			out = append(out, sensorProblem{key: "min:" + sn.String(),
				msg: msg("sensor_below_min", params(l.Min), "%s %.1f %s is below its minimum of %.1f %s", sn.Label(), v, sn.Unit(), l.Min, sn.Unit())})
		}
	}
	for _, sn := range allPressureSensors {
		v, err := c.io.ReadPressureBar(sn)
		checkValue(sn, v, err)
	}
	for _, sn := range allTempSensors {
		v, err := c.io.ReadTempC(sn)
		checkValue(sn, v, err)
	}
	nozzles := c.live.Snapshot().Nozzles
	for _, d := range allDigitalInputs {
		// Nozzle sensors only matter when they're in use - unwired
		// ones mustn't stop a system that selects the nozzle by button.
		if (d == Nozzle350InUse || d == Nozzle700InUse) && !nozzles.SensorsEnabled {
			continue
		}
		if d == Nozzle700InUse && !nozzles.Has700Bar {
			continue
		}
		if _, err := c.io.ReadDigitalInput(d); err != nil {
			unreadable(d.String(), d.Label(), err)
		}
	}
	return out
}

// sensorsReady returns an error describing the first sensor problem
// (including one still waiting for its first reading), or nil if every
// sensor is readable and within limits - checked before anything is
// allowed to start a pump.
func (c *Controller) sensorsReady() error {
	if problems := c.checkSensors(); len(problems) > 0 {
		p := problems[0].msg
		return msgErr("cannot_start", map[string]any{"reason": p}, "cannot start: %s", p.Text)
	}
	return nil
}

func (c *Controller) superviseSafety() {
	current := map[string]string{}
	for _, p := range c.checkSensors() {
		if !p.pending {
			current[p.key] = p.msg.Text
			c.raiseFaultKey(p.key, emergencyStop(p.msg))
		}
	}
	// Log each sensor problem clearing, so the journal shows when a
	// sensor came back as well as when it failed - the fault itself
	// stays active until an operator clears it.
	for key, text := range c.sensorProblems {
		if _, still := current[key]; !still {
			log.Printf("Sensor problem resolved: %s", text)
		}
	}
	c.sensorProblems = current

	if level, err := c.io.ReadDigitalInput(HydraulicOilLevel); err == nil && level {
		c.raiseFaultKey("oil_level_low", emergencyStop(msg("oil_level_low", nil, "Hydraulic oil level low")))
	}

	// Hydrogen detected stops the booster; VentFan is driven separately
	// by tickFans and deliberately keeps running through the fault.
	if h2, err := c.io.ReadDigitalInput(HydrogenDetected); err == nil && h2 {
		c.raiseFaultKey("hydrogen_detected", emergencyStop(msg("hydrogen_detected", nil, "Hydrogen detected")))
	}

	// Motor-powered feedback is active-low: it should read low while its
	// pump is energised (confirming the motor is running) and high while
	// de-energised (confirming it's stopped). A fault is when the relay
	// and the input agree instead of mirroring each other - pump on with
	// the input still high (motor never confirmed running), or pump off
	// with the input still low (motor never confirmed stopped).
	pumpsMotors := []struct {
		pump  RelayOutput
		motor DigitalInput
		label string
	}{
		{Pump1, Motor1Powered, "1"},
		{Pump2, Motor2Powered, "2"},
	}
	for _, pm := range pumpsMotors {
		pumpOn, err := c.io.RelayState(pm.pump)
		if err != nil {
			continue
		}
		motorHigh, err := c.io.ReadDigitalInput(pm.motor)
		if err != nil {
			continue
		}
		if pumpOn == motorHigh {
			c.raiseFaultKey("pump_motor_mismatch:"+pm.label, emergencyStop(msg("pump_motor_mismatch", map[string]any{"pump": pm.label},
				"Pump %s / Motor %s feedback mismatch", pm.label, pm.label)))
		}
	}
}

// tickFans drives the two auxiliary relay outputs that aren't part of
// the hydraulic stage machine or the fill-job phase machine - they run
// continuously off live pump/temperature/gas-detector readings
// regardless of job phase, so an emergency stop or fault deliberately
// does not force them off (see raiseFault/Stop): VentFan keeps purging
// the enclosure and CoolingFan keeps cooling the oil for as long as
// either is actually warranted. "Operating" here means either pump is
// commanded on - that covers fills, homing and manual piston control
// alike, since nothing moves hydraulically without a pump.
//
//   - VentFan energises the instant either pump is commanded on or
//     HydrogenDetected reads high, and de-energises VentFan.OffDelay
//     after both pumps are off and the detector has cleared. A failed
//     detector read counts as hydrogen present (fail-safe: ventilate
//     when the area can't be confirmed clear).
//   - CoolingFan runs whenever either pump is on. Independently of
//     pump state, it also turns on once oil exceeds the setpoint and
//     stays on by temperature until the oil drops to setpoint-1, so
//     residual heat keeps getting cooled after the pump that generated
//     it stops. A failed oil temperature read holds the temperature
//     demand at its last state rather than guessing either way.
func (c *Controller) tickFans(now time.Time) {
	p1, _ := c.io.RelayState(Pump1)
	p2, _ := c.io.RelayState(Pump2)
	anyPumpOn := p1 || p2

	h2, err := c.io.ReadDigitalInput(HydrogenDetected)
	h2Present := h2 || err != nil

	if anyPumpOn || h2Present {
		c.mu.Lock()
		c.ventDemandOffSince = time.Time{}
		c.mu.Unlock()
		_ = c.io.SetRelay(VentFan, true)
	} else {
		c.mu.Lock()
		if c.ventDemandOffSince.IsZero() {
			c.ventDemandOffSince = now
		}
		elapsed := now.Sub(c.ventDemandOffSince)
		c.mu.Unlock()
		if elapsed >= c.live.Snapshot().VentFan.OffDelay {
			_ = c.io.SetRelay(VentFan, false)
		}
	}

	oilT, oilErr := c.io.ReadTempC(HydOilTemp)
	c.mu.Lock()
	hot := c.oilAboveSetpoint
	if oilErr == nil {
		setpoint := c.live.Snapshot().CoolingFan.SetpointC
		switch {
		case !hot && oilT > setpoint:
			hot = true
		case hot && oilT < setpoint-1:
			hot = false
		}
	}
	c.oilAboveSetpoint = hot
	c.mu.Unlock()
	_ = c.io.SetRelay(CoolingFan, anyPumpOn || hot)
}

// raiseFault records a fault from plain (untranslated) text.
func (c *Controller) raiseFault(text string) {
	c.raiseFaultKey(text, Message{Text: text})
}

// emergencyStop wraps the reason for a fault that stops the booster.
func emergencyStop(reason Message) Message {
	return msg("emergency_stop", map[string]any{"reason": reason}, "%s - emergency stop", reason.Text)
}

// FaultRecord is one entry in Status.FaultDetails - the same fault as
// the matching Status.Faults string, with its Message for translation.
type FaultRecord struct {
	At time.Time `json:"at"`
	Message
}

// raiseFaultKey records a fault under key - a condition that persists
// across many control-loop ticks (e.g. an unreadable sensor) is only
// added to the fault list once until the fault is cleared, rather than
// once per tick. Every distinct key is still recorded, so all of the
// sensors that are currently a problem show up.
func (c *Controller) raiseFaultKey(key string, m Message) {
	c.mu.Lock()
	already := c.phase == PhaseFault
	if already && c.faultKeys[key] {
		c.mu.Unlock()
		return
	}
	if c.faultKeys == nil {
		c.faultKeys = map[string]bool{}
	}
	c.faultKeys[key] = true
	c.faults = append(c.faults, FaultRecord{At: time.Now(), Message: m})
	c.phase = PhaseFault
	c.mu.Unlock()
	logErrorf("FAULT: %s", m.Text)
	if !already {
		c.stages.Stop()
		_ = c.io.SetRelay(VentValve, false)
		setIORunning(c.io, false)
		_ = c.io.SetRelay(Pump1, false)
		_ = c.io.SetRelay(Pump2, false)
		mass := c.deliveredMassKg()
		c.mu.Lock()
		c.finalDeliveredMassKg = mass
		c.mu.Unlock()
	}
}

// setIORunning tells io it's running (or not) if it supports RunnableIO
// (see io.go) - a no-op for real hardware, but stops SimulatedIO's
// physics drifting on forever between jobs.
func setIORunning(io IO, running bool) {
	if r, ok := io.(RunnableIO); ok {
		r.SetRunning(running)
	}
}

// Start validates and begins a fill job.
//
// NOTE ON SAFETY LIMITS: this software-level check is not a substitute
// for hardwired protection (pressure relief valves sized for the system,
// burst discs, and an E-stop circuit that is independent of the control
// software). Review the Safety limits (adjustable from the admin screen,
// backed by DefaultConfig() in config.go) against the actual skid's
// certified working pressures before commissioning.
func (c *Controller) Start(job JobRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.manualMode {
		return msgErr("manual_blocks_start", nil, "cannot start an automatic job while manual piston control is enabled")
	}
	if c.phase == PhaseFilling || c.phase == PhaseVenting {
		return msgErr("job_already_running", nil, "job already running")
	}
	nozzles := c.live.Snapshot().Nozzles
	if nozzles.SensorsEnabled {
		// The lifted nozzle decides - a request naming a different one
		// (e.g. the screen hadn't caught up yet) is refused rather than
		// silently filling through a nozzle nobody chose.
		detected, err := c.detectNozzle()
		if err != nil {
			return err
		}
		if job.Nozzle != 0 && job.Nozzle != detected {
			return msgErr("nozzle_mismatch", map[string]any{"lifted": fmt.Sprint(int(detected)), "requested": fmt.Sprint(int(job.Nozzle))},
				"the %d bar nozzle is lifted, not the %d bar nozzle", detected, job.Nozzle)
		}
		job.Nozzle = detected
	}
	if job.Nozzle != Nozzle350 && job.Nozzle != Nozzle700 {
		return msgErr("nozzle_invalid", nil, "nozzle must be 350 or 700 bar")
	}
	if job.Nozzle == Nozzle700 && !nozzles.Has700Bar {
		return msgErr("nozzle_700_not_fitted", nil, "this system has no 700 bar nozzle")
	}
	maxForNozzle := float64(job.Nozzle)
	if job.TargetPressureBar <= 0 || job.TargetPressureBar > maxForNozzle {
		return msgErr("target_pressure_range", map[string]any{"max": fmt.Sprint(int(job.Nozzle))}, "target pressure must be between 0 and %d bar for the selected nozzle", job.Nozzle)
	}
	maxOutput := c.live.Snapshot().Safety.Limits[OutputPressure].Max
	if job.TargetPressureBar > maxOutput {
		return msgErr("target_pressure_over_limit", map[string]any{"limit": fmt.Sprintf("%.0f", maxOutput)}, "target pressure exceeds system safety limit of %.0f bar", maxOutput)
	}
	if job.TargetMassKg <= 0 {
		return msgErr("target_mass_invalid", nil, "target mass must be greater than zero")
	}
	if job.CustomerCode != "" && !isValidCustomerCode(job.CustomerCode) {
		return msgErr("customer_code_format", nil, "customer code must be 1-10 digits")
	}
	if job.CustomerCode != "" && c.validCodes != nil && !c.validCodes.IsValid(job.CustomerCode) {
		return msgErr("customer_code_unknown", nil, "customer code not recognised")
	}

	if err := c.sensorsReady(); err != nil {
		return err
	}

	// Resolve the login code to the customer, then drop it - from here
	// on the job is identified only by customer ID and name.
	var customer Customer
	if job.CustomerCode != "" && c.validCodes != nil {
		customer, _ = c.validCodes.Lookup(job.CustomerCode)
	}
	codeEntered := job.CustomerCode != ""
	job.CustomerCode = ""

	c.job = job
	c.customer = customer
	c.faults = nil
	c.faultKeys = nil
	c.finalDeliveredMassKg = 0
	c.startedAt = time.Now()
	tank := c.live.Snapshot().Tank
	if tp, err := c.io.ReadPressureBar(TankPressure); err == nil {
		c.initialTankKg = MassInVolumeKg(tank.VolumeLitres, tp, tank.NominalTempC)
	}
	// Best-effort: zero a real flow meter's totalizer for this fill, if
	// one is configured and active (see FlowMeterIO in io.go). A no-op
	// when absent/inactive - deliveredMassKg() falls back to the tank
	// estimate above in that case anyway.
	if fm, ok := c.io.(FlowMeterIO); ok {
		_ = fm.ResetFlowTotal()
	}
	c.phase = PhaseFilling
	setIORunning(c.io, true)
	_ = c.io.SetRelay(Pump1, true)
	_ = c.io.SetRelay(Pump2, true)

	c.stages.Start()
	log.Printf("Job started: target %.1f bar, %.3f kg, %d bar nozzle, %s",
		job.TargetPressureBar, job.TargetMassKg, job.Nozzle, describeCustomer(customer, codeEntered))
	return nil
}

// detectNozzle reads the nozzle proximity sensors (1 = lifted) and
// returns the one nozzle in use. It's an error for none, or both, to be
// lifted - only one nozzle can be in use at a time. On a system without
// a 700 bar nozzle only the 350 bar sensor is consulted.
func (c *Controller) detectNozzle() (NozzleSize, error) {
	has700 := c.live.Snapshot().Nozzles.Has700Bar
	in350, err := c.io.ReadDigitalInput(Nozzle350InUse)
	if err != nil {
		return 0, &msgError{Message: msg("sensor_unreadable", map[string]any{"sensor": Nozzle350InUse.String(), "reason": messageOf(err)},
			"%s sensor unreadable (%v)", Nozzle350InUse.Label(), err), wrapped: err}
	}
	in700 := false
	if has700 {
		if in700, err = c.io.ReadDigitalInput(Nozzle700InUse); err != nil {
			return 0, &msgError{Message: msg("sensor_unreadable", map[string]any{"sensor": Nozzle700InUse.String(), "reason": messageOf(err)},
				"%s sensor unreadable (%v)", Nozzle700InUse.Label(), err), wrapped: err}
		}
	}
	switch {
	case in350 && in700:
		return 0, msgErr("nozzle_both_lifted", nil, "both nozzles are lifted - return the one not being used")
	case in350:
		return Nozzle350, nil
	case in700:
		return Nozzle700, nil
	default:
		return 0, msgErr("nozzle_none_lifted", nil, "lift the nozzle to be used before starting")
	}
}

// NozzleStatus tells the job screens which nozzle options to offer and,
// with sensors enabled, which nozzle is currently lifted.
type NozzleStatus struct {
	Has700Bar      bool     `json:"has700Bar"`
	SensorsEnabled bool     `json:"sensorsEnabled"`
	Detected       int      `json:"detected"`          // 350 or 700 when exactly one nozzle is lifted, else 0
	Problem        *Message `json:"problem,omitempty"` // why no nozzle is detected (none/both lifted, sensor fault)
}

func (c *Controller) nozzleStatus() NozzleStatus {
	cfg := c.live.Snapshot().Nozzles
	st := NozzleStatus{Has700Bar: cfg.Has700Bar, SensorsEnabled: cfg.SensorsEnabled}
	if cfg.SensorsEnabled {
		if n, err := c.detectNozzle(); err != nil {
			m := messageOf(err)
			st.Problem = &m
		} else {
			st.Detected = int(n)
		}
	}
	return st
}

// describeCustomer is how a job's customer appears in the log - by ID
// and name, never by login code.
func describeCustomer(c Customer, codeEntered bool) string {
	switch {
	case c.ID != "":
		return fmt.Sprintf("customer %s (%s)", c.ID, c.Name)
	case codeEntered:
		return "customer code entered (no codes list to identify the customer)"
	default:
		return "no customer"
	}
}

// isValidCustomerCode checks a non-empty customer-identifying code is
// 1-10 ASCII digits - deliberately simple (no format assumptions beyond
// "digits", since it may be a membership number or other identifier).
// The public customer screen requires one before Start is enabled; it's
// optional everywhere else (e.g. the staff operator screen), so an empty
// string is handled by the caller, not this function.
func isValidCustomerCode(code string) bool {
	if len(code) < 1 || len(code) > 10 {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Stop is an operator abort: stages halt immediately, vent valve is
// closed, no automatic vent sequence runs (use the normal target-reached
// path for that).
func (c *Controller) Stop() {
	c.mu.Lock()
	wasFilling := c.phase == PhaseFilling || c.phase == PhaseVenting
	c.mu.Unlock()

	c.stages.Stop()
	_ = c.io.SetRelay(VentValve, false)
	setIORunning(c.io, false)
	_ = c.io.SetRelay(Pump1, false)
	_ = c.io.SetRelay(Pump2, false)
	// deliveredMassKg locks internally, so this must happen before the
	// lock below, not inside it (sync.Mutex isn't reentrant).
	mass := c.deliveredMassKg()

	c.mu.Lock()
	if wasFilling {
		c.phase = PhaseIdle
		c.finalDeliveredMassKg = mass
	}
	c.mu.Unlock()
	log.Printf("Job stopped by operator")
}

// ClearFault lets an operator acknowledge and clear a fault once the
// underlying condition has been resolved, returning the system to idle.
func (c *Controller) ClearFault() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.phase == PhaseFault {
		log.Printf("Fault cleared by operator (%d fault(s) acknowledged)", len(c.faults))
		c.phase = PhaseIdle
		c.faults = nil
		c.faultKeys = nil
	}
}

// ManualMode reports whether manual piston control is currently
// enabled (see SetManualMode).
func (c *Controller) ManualMode() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.manualMode
}

// SetManualMode enables or disables the manual piston control page.
// While enabled, the automatic job sequencer refuses to start (Start
// above) - manual control and the automatic stage machine must never
// drive the relays at once. Enabling is refused while a job is actually
// running (Filling/Venting); it's allowed from every other phase,
// including Fault, since manual control is also how an operator jogs a
// stuck ram clear by hand.
//
// Both entering and leaving manual mode drive every manually-controlled
// output to its safe default - relief valves B/D open, direction valves
// A/C off, both pumps off - the same state Stages.Start's homing step
// uses, so the system is always in a known state when manual control
// starts or hands back to automatic.
func (c *Controller) SetManualMode(enable bool) error {
	c.mu.Lock()
	if enable && (c.phase == PhaseFilling || c.phase == PhaseVenting) {
		c.mu.Unlock()
		return msgErr("manual_blocked_by_job", nil, "cannot enable manual control while a job is running")
	}
	c.manualMode = enable
	// Clear any pending reverse-delay close in the same critical section
	// as the mode flip, so tickManualJog can never close a relief valve
	// on behalf of a press that manual mode has already moved past.
	c.manualReverse = [2]manualReverseState{}
	c.mu.Unlock()

	snap := c.live.Snapshot()
	for _, cfg := range []StageConfig{snap.Stage1, snap.Stage2} {
		_ = c.io.SetRelay(cfg.DirRelay, false)
		_ = c.io.SetRelay(cfg.ReliefRelay, true)
	}
	_ = c.io.SetRelay(Pump1, false)
	_ = c.io.SetRelay(Pump2, false)
	return nil
}

// manualStageConfig resolves stage (1 or 2) to its StageConfig, shared
// by ManualSetPump/ManualJog below.
func (c *Controller) manualStageConfig(stage int) (StageConfig, error) {
	snap := c.live.Snapshot()
	switch stage {
	case 1:
		return snap.Stage1, nil
	case 2:
		return snap.Stage2, nil
	default:
		return StageConfig{}, msgErr("stage_invalid", nil, "stage must be 1 or 2")
	}
}

// ManualSetPump turns one stage's hydraulic pump motor on or off, from
// the manual control page. Only permitted while manual mode is enabled.
func (c *Controller) ManualSetPump(stage int, on bool) error {
	if !c.ManualMode() {
		return msgErr("manual_not_enabled", nil, "manual control is not enabled")
	}
	var pump RelayOutput
	switch stage {
	case 1:
		pump = Pump1
	case 2:
		pump = Pump2
	default:
		return msgErr("stage_invalid", nil, "stage must be 1 or 2")
	}
	if on {
		c.mu.Lock()
		faulted := c.phase == PhaseFault
		c.mu.Unlock()
		if faulted {
			return msgErr("pump_blocked_by_fault", nil, "cannot start a pump while a fault is active - clear the fault first")
		}
		if err := c.sensorsReady(); err != nil {
			return err
		}
	}
	return c.io.SetRelay(pump, on)
}

// setManualReverse records (or clears) stage idx's (0 or 1) pending
// reverse-delay state for tickManualJog.
func (c *Controller) setManualReverse(idx int, pending bool, deadline time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.manualReverse[idx] = manualReverseState{pending: pending, deadline: deadline}
}

// ManualJog drives one stage's cylinder while its Up/Down button is
// held, from the manual control page.
//
// active=true (button pressed) sets the stage's direction valve (A/C)
// to the requested direction. If that's actually a change from the
// valve's current position, the relief valve (B/D) is left open for
// ReverseDelay - the same settling time the automatic sequencer gives
// the direction valve between strokes, see Stages' doc comment - before
// tickManualJog closes it to let hydraulic pressure drive the ram; if
// the direction valve was already in the requested position, the relief
// valve closes immediately, since no settling time is needed.
// active=false (button released) re-opens the relief valve immediately
// and cancels any pending reverse-delay close, stopping the ram; the
// direction valve is left as last commanded, matching the automatic
// sequencer's own between-strokes behaviour - it has no effect while
// the relief valve is open. Only permitted while manual mode is
// enabled.
func (c *Controller) ManualJog(stage int, up bool, active bool) error {
	return c.manualJogAt(stage, up, active, time.Now())
}

func (c *Controller) manualJogAt(stage int, up bool, active bool, now time.Time) error {
	if !c.ManualMode() {
		return msgErr("manual_not_enabled", nil, "manual control is not enabled")
	}
	cfg, err := c.manualStageConfig(stage)
	if err != nil {
		return err
	}
	idx := stage - 1

	if !active {
		c.setManualReverse(idx, false, time.Time{})
		return c.io.SetRelay(cfg.ReliefRelay, true)
	}

	wasUp, err := c.io.RelayState(cfg.DirRelay)
	if err != nil {
		return err
	}
	if err := c.io.SetRelay(cfg.DirRelay, up); err != nil {
		return err
	}
	if wasUp == up {
		c.setManualReverse(idx, false, time.Time{})
		return c.io.SetRelay(cfg.ReliefRelay, false)
	}

	c.setManualReverse(idx, true, now.Add(cfg.ReverseDelay))
	return nil
}

// tickManualJog closes a stage's relief valve once a pending
// ManualJog-triggered direction reversal's ReverseDelay has elapsed -
// called once per control-loop tick from Run(), the same cadence as
// Stages.Tick/tickFans/superviseJob, so it needs no timer or goroutine
// of its own. A no-op for a stage with nothing pending (the common
// case, and always true outside manual mode - see SetManualMode, which
// clears this state the instant manual mode changes).
func (c *Controller) tickManualJog(now time.Time) {
	for idx, stage := range [2]int{1, 2} {
		c.mu.Lock()
		st := c.manualReverse[idx]
		c.mu.Unlock()
		if !st.pending || now.Before(st.deadline) {
			continue
		}
		cfg, err := c.manualStageConfig(stage)
		if err != nil {
			continue
		}
		c.setManualReverse(idx, false, time.Time{})
		_ = c.io.SetRelay(cfg.ReliefRelay, false)
	}
}

// deliveredMassKg prefers a real flow meter's totalizer (see
// FlowMeterIO in io.go) when one is configured and active, falling
// back to the tank pressure-differential estimate otherwise - the only
// two situations that "otherwise" covers (no meter selected, or the
// meter is selected but its last read errored) are treated identically
// on purpose, so a disconnected/misconfigured meter degrades to the
// existing estimate rather than reporting zero or stale data.
func (c *Controller) deliveredMassKg() float64 {
	if fm, ok := c.io.(FlowMeterIO); ok {
		if v, err := fm.ReadFlowTotalKg(); err == nil {
			return v
		}
	}
	tank := c.live.Snapshot().Tank
	tp, err := c.io.ReadPressureBar(TankPressure)
	if err != nil {
		return 0
	}
	remaining := MassInVolumeKg(tank.VolumeLitres, tp, tank.NominalTempC)
	c.mu.Lock()
	initial := c.initialTankKg
	c.mu.Unlock()
	d := initial - remaining
	if d < 0 {
		d = 0
	}
	return d
}

// superviseJob advances the filling -> (venting) -> complete sequence.
// Also refreshes the filtered output pressure reading (see
// tickOutputPressureFilter) - called once per control-loop tick
// regardless of phase, so Status()'s display value stays current even
// when idle, and so a direct call (as the tests do, without going
// through Run()) still sees a consistent, freshly-filtered value.
func (c *Controller) superviseJob() {
	c.tickOutputPressureFilter()

	c.mu.Lock()
	phase := c.phase
	job := c.job
	c.mu.Unlock()

	if phase != PhaseFilling {
		return
	}

	// With nozzle sensors in use, the nozzle the fill started on must
	// stay the only one lifted - a returned nozzle, or a second one
	// lifted, mid-fill stops it.
	if c.live.Snapshot().Nozzles.SensorsEnabled {
		if n, err := c.detectNozzle(); err != nil || n != job.Nozzle {
			reason := msg("nozzle_changed", map[string]any{"nozzle": fmt.Sprint(int(job.Nozzle))}, "the %d bar nozzle is no longer the only nozzle lifted", job.Nozzle)
			c.raiseFaultKey("nozzle_changed", emergencyStop(reason))
			return
		}
	}

	outP, err := c.outputPressureBar()
	if err != nil {
		return
	}
	mass := c.deliveredMassKg()
	if outP < job.TargetPressureBar && mass < job.TargetMassKg {
		return
	}

	c.stages.Stop()
	setIORunning(c.io, false)
	_ = c.io.SetRelay(Pump1, false)
	_ = c.io.SetRelay(Pump2, false)
	c.mu.Lock()
	c.finalDeliveredMassKg = mass
	c.mu.Unlock()

	// Without real mass instrumentation, DeliveredMassKg is a tank
	// pressure differential estimate, not a scale reading (see
	// MassInVolumeKg in gas.go) - not something to hand to a billing/
	// accounting system as fact, so skip recording it entirely.
	if c.dispenses != nil && c.live.Snapshot().MassMeasurementEnabled {
		c.mu.Lock()
		customer := c.customer
		c.mu.Unlock()
		if err := c.dispenses.Record(DispenseRecord{
			Timestamp: time.Now(), CustomerID: customer.ID, CustomerName: customer.Name, DeliveredMassKg: mass,
		}); err != nil {
			logErrorf("failed to record dispense: %v", err)
		}
	}

	if job.Nozzle == Nozzle700 {
		c.mu.Lock()
		c.phase = PhaseVenting
		c.mu.Unlock()
		_ = c.io.SetRelay(VentValve, true)
		ventDuration := c.live.Snapshot().Vent.Duration
		log.Printf("Target reached (%.1f bar, %.3f kg) - venting delivery hose for %s", outP, mass, ventDuration)
		go func() {
			time.Sleep(ventDuration)
			_ = c.io.SetRelay(VentValve, false)
			c.mu.Lock()
			c.phase = PhaseComplete
			c.mu.Unlock()
			log.Printf("Vent complete")
		}()
	} else {
		c.mu.Lock()
		c.phase = PhaseComplete
		c.mu.Unlock()
		log.Printf("Target reached (%.1f bar, %.3f kg) - no vent required for 350 bar nozzle", outP, mass)
	}
}

// allRelays/allPressureSensors/allTempSensors/allDigitalInputs enumerate
// every physical point on the skid, for the diagnostics page -
// independent of which ones a running job currently cares about.
var allRelays = []RelayOutput{Valve1A, Valve1B, Valve2A, Valve2B, VentValve, Pump1, Pump2, VentFan, CoolingFan}
var allPressureSensors = []SensorInput{Hyd1Pressure, Hyd2Pressure, TankPressure, InterStagePressure, OutputPressure}
var allTempSensors = []SensorInput{HydOilTemp, OutputTemp}

// allLimitSensors is every sensor with a min/max in SafetyConfig.Limits.
var allLimitSensors = append(append([]SensorInput{}, allPressureSensors...), allTempSensors...)
var allDigitalInputs = []DigitalInput{HydraulicOilLevel, Motor1Powered, Motor2Powered, HydrogenDetected, Nozzle350InUse, Nozzle700InUse}

type ValveStatus struct {
	Name string `json:"name"`
	On   bool   `json:"on"`
	Ok   bool   `json:"ok"` // false if the read errored (e.g. GPIO fault)
}

type SensorStatus struct {
	Name  string  `json:"name"`
	Kind  string  `json:"kind"` // "pressure" or "temp"
	Value float64 `json:"value"`
	Ok    bool    `json:"ok"` // false if the read errored (fault/not wired)
}

// DigitalInputStatus reports one feedback signal's raw electrical level -
// kept as its own array (rather than folded into SensorStatus.Kind)
// since it's a boolean level, not a calibrated numeric reading.
type DigitalInputStatus struct {
	Name string `json:"name"`
	High bool   `json:"high"`
	Ok   bool   `json:"ok"` // false if the read errored (fault/not wired)
}

type Diagnostics struct {
	Valves        []ValveStatus        `json:"valves"`
	Sensors       []SensorStatus       `json:"sensors"`
	DigitalInputs []DigitalInputStatus `json:"digitalInputs"`
}

// Diagnostics reads every valve and sensor directly from IO, bypassing
// job/phase state entirely - a live view of the raw hardware, useful for
// commissioning and wiring checks independent of whether a fill is
// running.
func (c *Controller) Diagnostics() Diagnostics {
	var d Diagnostics
	for _, r := range allRelays {
		on, err := c.io.RelayState(r)
		d.Valves = append(d.Valves, ValveStatus{Name: r.String(), On: on, Ok: err == nil})
	}
	for _, s := range allPressureSensors {
		v, err := c.io.ReadPressureBar(s)
		d.Sensors = append(d.Sensors, SensorStatus{Name: s.String(), Kind: "pressure", Value: v, Ok: err == nil})
	}
	for _, s := range allTempSensors {
		v, err := c.io.ReadTempC(s)
		d.Sensors = append(d.Sensors, SensorStatus{Name: s.String(), Kind: "temp", Value: v, Ok: err == nil})
	}
	for _, di := range allDigitalInputs {
		v, err := c.io.ReadDigitalInput(di)
		d.DigitalInputs = append(d.DigitalInputs, DigitalInputStatus{Name: di.String(), High: v, Ok: err == nil})
	}
	return d
}

func (c *Controller) Status() Status {
	c.mu.Lock()
	phase := c.phase
	job := c.job
	var faults []string
	for _, f := range c.faults {
		faults = append(faults, fmt.Sprintf("[%s] %s", f.At.Format(time.RFC3339), f.Text))
	}
	faultDetails := append([]FaultRecord{}, c.faults...)
	frozenMass := c.finalDeliveredMassKg
	manualMode := c.manualMode
	var started *time.Time
	if !c.startedAt.IsZero() {
		t := c.startedAt
		started = &t
	}
	c.mu.Unlock()

	outP, _ := c.outputPressureBar()
	outT, _ := c.io.ReadTempC(OutputTemp)
	tankP, _ := c.io.ReadPressureBar(TankPressure)
	hydOilT, _ := c.io.ReadTempC(HydOilTemp)
	vent, _ := c.io.RelayState(VentValve)

	elapsed := 0.0
	if started != nil {
		elapsed = time.Since(*started).Seconds()
	}

	// Once a job isn't actively filling, report the mass snapshot taken
	// the moment it stopped rather than keep recomputing it live -
	// otherwise the figure would keep drifting from ordinary sensor
	// noise (temperature jitter, on real hardware too) even though no
	// gas is moving anymore. Venting only bleeds the delivery hose -
	// the tank itself stopped being drawn from the moment the stages
	// stopped, just before venting began - so it's frozen too.
	deliveredMass := frozenMass
	if phase == PhaseFilling {
		deliveredMass = c.deliveredMassKg()
	}

	s1, s2 := c.stages.Status()
	return Status{
		Phase: phase, Running: phase == PhaseFilling || phase == PhaseVenting,
		Nozzle: int(job.Nozzle), TargetPressureBar: job.TargetPressureBar, TargetMassKg: job.TargetMassKg,
		OutputPressureBar: outP, OutputTempC: outT,
		TankPressureBar: tankP, HydOilTempC: hydOilT,
		DeliveredMassKg: deliveredMass,
		Stage1:          s1,
		Stage2:          s2,
		VentOpen:        vent, Faults: faults, FaultDetails: faultDetails, StartedAt: started, ElapsedSec: elapsed,
		ManualMode: manualMode,
		Nozzles:    c.nozzleStatus(),
	}
}
