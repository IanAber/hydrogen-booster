package main

// CompositeIO layers the real driver modules (PressureModule for the CAN
// pressure input module, TemperatureModule for the DS18B20 1-Wire
// sensors, RelayModule for the MCP23017 relay outputs) over a base IO
// implementation (SimulatedIO today; more real drivers can be layered in
// the same way as they're supplied) so the rest of the system - Stage,
// Controller, the web UI - keeps using the plain IO interface without
// knowing which points are real and which are still simulated.
type CompositeIO struct {
	base IO
	pm   *PressureModule
	tm   *TemperatureModule
	rm   *RelayModule
	dim  *DigitalInputModule
	fm   *FlowModule
}

func NewCompositeIO(base IO, pm *PressureModule, tm *TemperatureModule, rm *RelayModule, dim *DigitalInputModule, fm *FlowModule) *CompositeIO {
	return &CompositeIO{base: base, pm: pm, tm: tm, rm: rm, dim: dim, fm: fm}
}

func (c *CompositeIO) Close() error { return c.base.Close() }

// SetRunning implements RunnableIO by forwarding to the base IO if it
// supports being told the job is running (see RunnableIO in io.go) -
// only SimulatedIO cares; a real driver's readings just reflect reality
// and need no such signal.
func (c *CompositeIO) SetRunning(running bool) {
	if r, ok := c.base.(RunnableIO); ok {
		r.SetRunning(running)
	}
}

// SetRelay always mirrors into the base simulated model too, regardless
// of whether the real relay module is live for this valve - that keeps
// SimulatedIO's own hydraulic-pressure physics (which reacts to
// direction-valve changes) coherent for any pressure/temp sensor still
// running in simulation, even while relays are already live. It also
// mirrors a pump command into the digital input module's own simulated
// value for that pump's motor-powered feedback signal - active-low, so
// the mirrored value is the command's inverse - (best-effort; ignored
// if that module is live or the signal isn't assigned), so the admin
// screen's digital input panel reflects a healthy pump/motor pair
// without an operator having to touch it first - the same "healthy
// simulated motor" default SimulatedIO.SetRelay provides when this
// module is the one actually serving that signal.
func (c *CompositeIO) SetRelay(r RelayOutput, on bool) error {
	baseErr := c.base.SetRelay(r, on)
	if r == Pump1 {
		_ = c.dim.SetSimValue(Motor1Powered, !on)
	}
	if r == Pump2 {
		_ = c.dim.SetSimValue(Motor2Powered, !on)
	}
	if handled, err := c.rm.SetRelay(r, on); handled {
		return err
	}
	return baseErr
}

// RelayState serves the real relay module's reading for any valve
// assigned to one of its enabled channels while it's live, and falls
// back to the base IO (simulated) otherwise.
func (c *CompositeIO) RelayState(r RelayOutput) (bool, error) {
	if on, handled, err := c.rm.RelayState(r); handled {
		return on, err
	}
	return c.base.RelayState(r)
}

// ReadPressureBar serves the CAN pressure module's reading whenever
// that module is responsible for s - every pressure sensor while it's
// live, including its error if the reading is missing or stale - and
// only falls back to the base IO (simulated) otherwise. A live sensor
// that stops reading must surface as an error, never as a plausible
// simulated value.
func (c *CompositeIO) ReadPressureBar(s SensorInput) (float64, error) {
	if v, handled, err := c.pm.readForIO(s); handled {
		return v, err
	}
	return c.base.ReadPressureBar(s)
}

// ReadTempC follows the same rules as ReadPressureBar, for the 1-Wire
// temperature module.
func (c *CompositeIO) ReadTempC(s SensorInput) (float64, error) {
	if v, handled, err := c.tm.readForIO(s); handled {
		return v, err
	}
	return c.base.ReadTempC(s)
}

// ReadDigitalInput follows the same rules as ReadPressureBar, for the
// GPIO digital input module.
func (c *CompositeIO) ReadDigitalInput(d DigitalInput) (bool, error) {
	if v, handled, err := c.dim.readForIO(d); handled {
		return v, err
	}
	return c.base.ReadDigitalInput(d)
}

// ReadFlowTotalKg/ResetFlowTotal implement the optional FlowMeterIO
// capability (see io.go) by delegating to the flow module - there is no
// base/simulated fallback here (unlike every ReadX method above): with
// no real sensor selected/active, FlowModule.ReadTotalKg reports
// ok=false and Controller falls back to the tank pressure-differential
// estimate itself, via the same FlowMeterIO type assertion.
func (c *CompositeIO) ReadFlowTotalKg() (float64, error) {
	if v, ok := c.fm.ReadTotalKg(); ok {
		return v, nil
	}
	return 0, ErrSensorFault
}

func (c *CompositeIO) ResetFlowTotal() error {
	_, err := c.fm.ResetTotal()
	return err
}
