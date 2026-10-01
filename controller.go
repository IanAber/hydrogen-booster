package main

import (
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
	// CustomerCode identifies who a fill was for, entered on the public
	// customer screen before Start is enabled there. 1-10 digits,
	// required. Purely a record for the journal (see the "Job started"
	// log line); nothing downstream acts on it.
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
	StartedAt         *time.Time  `json:"startedAt,omitempty"`
	ElapsedSec        float64     `json:"elapsedSec"`
	ManualMode        bool        `json:"manualMode"`
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
	faults        []string
	// finalDeliveredMassKg is the delivered-mass snapshot taken at the
	// moment a job stops (reaches target, is aborted, or faults).
	// Status() reports this instead of a fresh deliveredMassKg() call
	// once the job isn't actively running - otherwise the figure would
	// keep drifting from ordinary sensor noise (temperature jitter, on
	// real hardware too) even though no gas is moving anymore.
	finalDeliveredMassKg float64

	// pumpsOffSince/coolingFanOn are tickFans' own persistent state -
	// zero pumpsOffSince means the pumps are currently on (or have never
	// been observed off); coolingFanOn is the fan's last commanded
	// state, needed because its own off-transition depends on that
	// history, not just the instantaneous temperature reading.
	pumpsOffSince time.Time
	coolingFanOn  bool

	// outputPressureFilterBuf/filteredOutputPressureBar/
	// filteredOutputPressureErr are tickOutputPressureFilter's own
	// persistent state - refreshed once per control-loop tick so every
	// consumer (superviseJob, Status) sees the same filtered reading
	// regardless of how often each is separately invoked (e.g. an HTTP
	// status poll landing between ticks) - see tickOutputPressureFilter.
	outputPressureFilterBuf   []float64
	filteredOutputPressureBar float64
	filteredOutputPressureErr error

	// dispenseLog and validCodes are optional (nil-safe) - set via
	// SetDispenseLog/SetValidCodes if the caller wants completed fills
	// recorded and customer codes checked against an allow-list. Kept
	// optional (rather than required constructor args) so existing
	// callers/tests that don't care about either keep working unchanged.
	dispenseLog *DispenseLog
	validCodes  *ValidCodes

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

// SetDispenseLog wires up recording of completed fills (see
// superviseJob). Call once before Run(); nil is fine and just means
// completions aren't recorded anywhere.
func (c *Controller) SetDispenseLog(d *DispenseLog) {
	c.dispenseLog = d
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
// Deliberately NOT used by superviseSafety's MaxHydrogenPressureBar
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

func (c *Controller) superviseSafety() {
	safety := c.live.Snapshot().Safety
	check := func(label string, v float64, max float64) {
		if v > max {
			c.raiseFault(fmt.Sprintf("%s %.1f exceeds limit %.1f - emergency stop", label, v, max))
		}
	}
	if p, err := c.io.ReadPressureBar(Hyd1Pressure); err == nil {
		check("Hydraulic 1 pressure (bar)", p, safety.MaxHydraulicPressureBar)
	}
	if p, err := c.io.ReadPressureBar(Hyd2Pressure); err == nil {
		check("Hydraulic 2 pressure (bar)", p, safety.MaxHydraulicPressureBar)
	}
	if p, err := c.io.ReadPressureBar(OutputPressure); err == nil {
		check("Hydrogen output pressure (bar)", p, safety.MaxHydrogenPressureBar)
	}
	for _, s := range []SensorInput{HydOilTemp, OutputTemp} {
		if temp, err := c.io.ReadTempC(s); err == nil {
			check("Temperature (C)", temp, safety.MaxTempC)
		}
	}

	if level, err := c.io.ReadDigitalInput(HydraulicOilLevel); err == nil && level {
		c.raiseFault("Hydraulic oil level low - emergency stop")
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
			c.raiseFault(fmt.Sprintf("Pump %s / Motor %s feedback mismatch - emergency stop", pm.label, pm.label))
		}
	}
}

// tickFans drives the two auxiliary relay outputs that aren't part of
// the hydraulic stage machine or the fill-job phase machine - they run
// continuously off live pump/temperature readings regardless of job
// phase, so an emergency stop or fault deliberately does not force
// them off (see raiseFault/Stop): VentFan keeps purging the enclosure
// and CoolingFan keeps cooling the oil for as long as either is
// actually warranted.
//
//   - VentFan energises the instant either pump is commanded on, and
//     de-energises VentFan.OffDelay after both are confirmed off.
//   - CoolingFan's turn-on requires both "oil above setpoint" AND
//     "pumps running"; once on, it stays on by temperature alone
//     (independent of pump state) until the oil drops to setpoint-1,
//     so residual heat keeps getting cooled after the pump that
//     generated it stops.
func (c *Controller) tickFans(now time.Time) {
	p1, _ := c.io.RelayState(Pump1)
	p2, _ := c.io.RelayState(Pump2)
	anyPumpOn := p1 || p2

	if anyPumpOn {
		c.mu.Lock()
		c.pumpsOffSince = time.Time{}
		c.mu.Unlock()
		_ = c.io.SetRelay(VentFan, true)
	} else {
		c.mu.Lock()
		if c.pumpsOffSince.IsZero() {
			c.pumpsOffSince = now
		}
		elapsed := now.Sub(c.pumpsOffSince)
		c.mu.Unlock()
		if elapsed >= c.live.Snapshot().VentFan.OffDelay {
			_ = c.io.SetRelay(VentFan, false)
		}
	}

	if oilT, err := c.io.ReadTempC(HydOilTemp); err == nil {
		setpoint := c.live.Snapshot().CoolingFan.SetpointC
		c.mu.Lock()
		on := c.coolingFanOn
		switch {
		case !on && anyPumpOn && oilT > setpoint:
			on = true
		case on && oilT < setpoint-1:
			on = false
		}
		c.coolingFanOn = on
		c.mu.Unlock()
		_ = c.io.SetRelay(CoolingFan, on)
	}
}

func (c *Controller) raiseFault(msg string) {
	c.mu.Lock()
	already := c.phase == PhaseFault
	c.faults = append(c.faults, fmt.Sprintf("[%s] %s", time.Now().Format(time.RFC3339), msg))
	c.phase = PhaseFault
	c.mu.Unlock()
	if !already {
		log.Printf("FAULT: %s", msg)
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
		return fmt.Errorf("cannot start an automatic job while manual piston control is enabled")
	}
	if c.phase == PhaseFilling || c.phase == PhaseVenting {
		return fmt.Errorf("job already running")
	}
	if job.Nozzle != Nozzle350 && job.Nozzle != Nozzle700 {
		return fmt.Errorf("nozzle must be 350 or 700 bar")
	}
	maxForNozzle := float64(job.Nozzle)
	if job.TargetPressureBar <= 0 || job.TargetPressureBar > maxForNozzle {
		return fmt.Errorf("target pressure must be between 0 and %d bar for the selected nozzle", job.Nozzle)
	}
	safety := c.live.Snapshot().Safety
	if job.TargetPressureBar > safety.MaxHydrogenPressureBar {
		return fmt.Errorf("target pressure exceeds system safety limit of %.0f bar", safety.MaxHydrogenPressureBar)
	}
	if job.TargetMassKg <= 0 {
		return fmt.Errorf("target mass must be greater than zero")
	}
	if job.CustomerCode != "" && !isValidCustomerCode(job.CustomerCode) {
		return fmt.Errorf("customer code must be 1-10 digits")
	}
	if job.CustomerCode != "" && c.validCodes != nil && !c.validCodes.IsValid(job.CustomerCode) {
		return fmt.Errorf("customer code not recognised")
	}

	c.job = job
	c.faults = nil
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
	codeMsg := "no customer code"
	if job.CustomerCode != "" {
		codeMsg = "customer code " + job.CustomerCode
	}
	log.Printf("Job started: target %.1f bar, %.3f kg, %d bar nozzle, %s",
		job.TargetPressureBar, job.TargetMassKg, job.Nozzle, codeMsg)
	return nil
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
		c.phase = PhaseIdle
		c.faults = nil
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
		return fmt.Errorf("cannot enable manual control while a job is running")
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
		return StageConfig{}, fmt.Errorf("stage must be 1 or 2")
	}
}

// ManualSetPump turns one stage's hydraulic pump motor on or off, from
// the manual control page. Only permitted while manual mode is enabled.
func (c *Controller) ManualSetPump(stage int, on bool) error {
	if !c.ManualMode() {
		return fmt.Errorf("manual control is not enabled")
	}
	var pump RelayOutput
	switch stage {
	case 1:
		pump = Pump1
	case 2:
		pump = Pump2
	default:
		return fmt.Errorf("stage must be 1 or 2")
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
		return fmt.Errorf("manual control is not enabled")
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
	if c.dispenseLog != nil && c.live.Snapshot().MassMeasurementEnabled {
		if err := c.dispenseLog.Record(DispenseRecord{
			Timestamp: time.Now(), CustomerCode: job.CustomerCode, DeliveredMassKg: mass,
		}); err != nil {
			log.Printf("failed to record dispense: %v", err)
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
var allDigitalInputs = []DigitalInput{HydraulicOilLevel, Motor1Powered, Motor2Powered}

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
	faults := append([]string{}, c.faults...)
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
		VentOpen:        vent, Faults: faults, StartedAt: started, ElapsedSec: elapsed,
		ManualMode: manualMode,
	}
}
