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
	// CustomerCode identifies who a fill was for - typically a phone
	// number, entered on the public customer screen before Start is
	// enabled there. 1-10 digits, required. Purely a record for the
	// journal (see the "Job started" log line); nothing downstream acts
	// on it.
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
	TankTempC         float64     `json:"tankTempC"`
	DeliveredMassKg   float64     `json:"deliveredMassKg"`
	Stage1            StageStatus `json:"stage1"`
	Stage2            StageStatus `json:"stage2"`
	VentOpen          bool        `json:"ventOpen"`
	Faults            []string    `json:"faults,omitempty"`
	StartedAt         *time.Time  `json:"startedAt,omitempty"`
	ElapsedSec        float64     `json:"elapsedSec"`
}

// Controller is the top level supervisor: it runs the two Stages, keeps
// them synchronised, enforces safety interlocks, and sequences a fill job
// from Start through target-reached, optional vent, to Complete. All
// tuning comes from LiveConfig, so it can be changed on the fly (e.g.
// from the admin web screen) without restarting.
type Controller struct {
	live *LiveConfig
	io   IO

	stage1 *Stage
	stage2 *Stage

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

	// dispenseLog and validCodes are optional (nil-safe) - set via
	// SetDispenseLog/SetValidCodes if the caller wants completed fills
	// recorded and customer codes checked against an allow-list. Kept
	// optional (rather than required constructor args) so existing
	// callers/tests that don't care about either keep working unchanged.
	dispenseLog *DispenseLog
	validCodes  *ValidCodes
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
	c.stage1 = NewStage(live, 1, io)
	c.stage2 = NewStage(live, 2, io)

	// Synchronisation: Stage 2 is fed by Stage 1's output, so it must not
	// be allowed to run away from (or lag too far behind) Stage 1. Stage 2
	// simply holds its current direction-valve state, mid-dwell, if
	// starting a new stroke would put it more than MaxSyncStrokeLead
	// strokes ahead of Stage 1; it re-checks every tick and proceeds as
	// soon as Stage 1 catches up. MaxSyncStrokeLead is itself live-tunable.
	c.stage1.AllowReverse = func(s *Stage) bool { return true }
	c.stage2.AllowReverse = func(s *Stage) bool {
		lead := int64(c.stage2.StrokeCount()) - int64(c.stage1.StrokeCount())
		return lead < int64(c.live.Snapshot().Safety.MaxSyncStrokeLead)
	}
	c.stage1.OnReversal = func(s *Stage) {
		st := s.Status()
		log.Printf("%s reversed -> %s (stroke %d)", st.Name, st.Direction, st.StrokeCount)
	}
	c.stage2.OnReversal = func(s *Stage) {
		st := s.Status()
		log.Printf("%s reversed -> %s (stroke %d)", st.Name, st.Direction, st.StrokeCount)
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
		c.stage1.Tick(now)
		c.stage2.Tick(now)
		c.superviseSafety()
		c.superviseJob()

		if newPeriod := c.live.Snapshot().PollPeriod; newPeriod != period && newPeriod > 0 {
			t.Stop()
			t = time.NewTicker(newPeriod)
			period = newPeriod
		}
	}
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
	for _, s := range []SensorInput{Hyd1Temp, Hyd2Temp, OutputTemp} {
		if temp, err := c.io.ReadTempC(s); err == nil {
			check("Temperature (C)", temp, safety.MaxTempC)
		}
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
		c.stage1.Stop()
		c.stage2.Stop()
		_ = c.io.SetRelay(VentValve, false)
		setIORunning(c.io, false)
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
		if tt, err2 := c.io.ReadTempC(TankTemp); err2 == nil {
			c.initialTankKg = MassInVolumeKg(tank.VolumeLitres, tp, tt)
		}
	}
	c.phase = PhaseFilling
	setIORunning(c.io, true)

	c.stage1.Start()
	c.stage2.Start()
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
// "digits", since it may be a phone number, a membership number, etc.).
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

	c.stage1.Stop()
	c.stage2.Stop()
	_ = c.io.SetRelay(VentValve, false)
	setIORunning(c.io, false)
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

func (c *Controller) deliveredMassKg() float64 {
	tank := c.live.Snapshot().Tank
	tp, err := c.io.ReadPressureBar(TankPressure)
	if err != nil {
		return 0
	}
	tt, err := c.io.ReadTempC(TankTemp)
	if err != nil {
		return 0
	}
	remaining := MassInVolumeKg(tank.VolumeLitres, tp, tt)
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
func (c *Controller) superviseJob() {
	c.mu.Lock()
	phase := c.phase
	job := c.job
	c.mu.Unlock()

	if phase != PhaseFilling {
		return
	}

	outP, err := c.io.ReadPressureBar(OutputPressure)
	if err != nil {
		return
	}
	mass := c.deliveredMassKg()
	if outP < job.TargetPressureBar && mass < job.TargetMassKg {
		return
	}

	c.stage1.Stop()
	c.stage2.Stop()
	setIORunning(c.io, false)
	c.mu.Lock()
	c.finalDeliveredMassKg = mass
	c.mu.Unlock()

	if c.dispenseLog != nil {
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

// allRelays/allPressureSensors/allTempSensors enumerate every physical
// point on the skid, for the diagnostics page - independent of which
// ones a running job currently cares about.
var allRelays = []RelayOutput{Valve1A, Valve1B, Valve2A, Valve2B, VentValve}
var allPressureSensors = []SensorInput{Hyd1Pressure, Hyd2Pressure, TankPressure, InterStagePressure, OutputPressure}
var allTempSensors = []SensorInput{Hyd1Temp, Hyd2Temp, TankTemp, InterStageTemp, OutputTemp}

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

type Diagnostics struct {
	Valves  []ValveStatus  `json:"valves"`
	Sensors []SensorStatus `json:"sensors"`
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
	return d
}

func (c *Controller) Status() Status {
	c.mu.Lock()
	phase := c.phase
	job := c.job
	faults := append([]string{}, c.faults...)
	frozenMass := c.finalDeliveredMassKg
	var started *time.Time
	if !c.startedAt.IsZero() {
		t := c.startedAt
		started = &t
	}
	c.mu.Unlock()

	outP, _ := c.io.ReadPressureBar(OutputPressure)
	outT, _ := c.io.ReadTempC(OutputTemp)
	tankP, _ := c.io.ReadPressureBar(TankPressure)
	tankT, _ := c.io.ReadTempC(TankTemp)
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

	return Status{
		Phase: phase, Running: phase == PhaseFilling || phase == PhaseVenting,
		Nozzle: int(job.Nozzle), TargetPressureBar: job.TargetPressureBar, TargetMassKg: job.TargetMassKg,
		OutputPressureBar: outP, OutputTempC: outT,
		TankPressureBar: tankP, TankTempC: tankT,
		DeliveredMassKg: deliveredMass,
		Stage1:          c.stage1.Status(),
		Stage2:          c.stage2.Status(),
		VentOpen:        vent, Faults: faults, StartedAt: started, ElapsedSec: elapsed,
	}
}
