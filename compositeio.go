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
}

func NewCompositeIO(base IO, pm *PressureModule, tm *TemperatureModule, rm *RelayModule) *CompositeIO {
	return &CompositeIO{base: base, pm: pm, tm: tm, rm: rm}
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
// running in simulation, even while relays are already live.
func (c *CompositeIO) SetRelay(r RelayOutput, on bool) error {
	baseErr := c.base.SetRelay(r, on)
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

// ReadPressureBar serves the CAN pressure module's reading for any
// sensor assigned to one of its enabled channels, and falls back to the
// base IO (still simulated, pending future real drivers) for anything
// not wired to it.
func (c *CompositeIO) ReadPressureBar(s SensorInput) (float64, error) {
	if v, ok := c.pm.ReadPressureBar(s); ok {
		return v, nil
	}
	return c.base.ReadPressureBar(s)
}

// ReadTempC serves the 1-Wire temperature module's reading for any
// sensor assigned to one of its enabled channels, and falls back to the
// base IO (still simulated) for anything not wired to it.
func (c *CompositeIO) ReadTempC(s SensorInput) (float64, error) {
	if v, ok := c.tm.ReadTempC(s); ok {
		return v, nil
	}
	return c.base.ReadTempC(s)
}
