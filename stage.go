package main

import (
	"sort"
	"sync"
	"time"
)

type Direction int

const (
	DirDown Direction = iota
	DirUp
)

type pSample struct {
	t time.Time
	p float64
}

// circuitPhase tracks where one hydraulic circuit (direction valve +
// relief valve) is in the synchronized stroke cycle. See Stages' doc
// comment for the full sequence.
type circuitPhase int

const (
	phaseHoming    circuitPhase = iota // system startup only: relief open, direction forced off
	phaseDriving                       // relief closed, driving, watching for this circuit's own spike/timeout
	phaseAtBarrier                     // this circuit finished its stroke; relief open, waiting for the other circuit
	phaseReversing                     // direction valve just flipped; relief still open, waiting ReverseDelay before closing it
)

// circuit holds one hydraulic circuit's mutable state. cfg is read
// fresh from LiveConfig on every tick (not cached here) so admin
// changes take effect immediately, matching the rest of this codebase.
type circuit struct {
	which       int // 1 or 2, selects Stage1/Stage2 from LiveConfig
	dir         Direction
	phase       circuitPhase
	phaseStart  time.Time
	history     []pSample
	filterBuf   []float64 // raw-sample ring buffer for the median filter
	strokeCount uint64
	lastFault   string
}

// median returns the middle value of vals (average of the two middle
// values for an even count). vals is copied before sorting, so the
// caller's slice order is left untouched.
func median(vals []float64) float64 {
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func (c *circuit) cfg(live *LiveConfig) StageConfig {
	snap := live.Snapshot()
	if c.which == 1 {
		return snap.Stage1
	}
	return snap.Stage2
}

// Stages drives both hydraulic circuits (Stage 1 = direction valve A +
// relief valve B, Stage 2 = direction valve C + relief valve D) as one
// tightly synchronized unit, per the confirmed physical model:
//
//   - A/C hold whatever direction they're commanded to, but the ram
//     can't physically follow that command while its circuit is under
//     pressure - the relief valve (B for circuit 1, D for circuit 2)
//     must be open for the direction change to take mechanical effect.
//   - Homing (Start): both relief valves open, both direction valves
//     forced off, held for Config.Homing.Duration so both rams settle
//     to the same physical reference end regardless of where they were
//     left by a previous run.
//   - Per stroke: close a circuit's relief valve (pressure builds, ram
//     drives) -> wait for that circuit's own pressure spike or a
//     MaxStrokeTime timeout -> re-open that circuit's relief valve.
//   - Barrier: once a circuit has re-opened its relief valve, it waits
//     until the OTHER circuit has also re-opened its relief valve
//     before either one's direction valve flips - so both cylinders
//     always reverse together, in lockstep, never more than one
//     circuit's stroke apart.
//   - After both direction valves flip, hold for ReverseDelay (letting
//     the direction valve mechanically settle) before closing both
//     relief valves again and starting the next stroke.
type Stages struct {
	live *LiveConfig
	io   IO

	mu          sync.Mutex
	running     bool
	homingStart time.Time
	lastSwitch  time.Time // last time any relay changed on either circuit; anchors PressureBlankTime
	c1, c2      circuit

	// OnReversal fires (outside the lock) once both circuits reverse
	// together - used for telemetry/logging.
	OnReversal func(dir Direction, strokeCount uint64)
}

func NewStages(live *LiveConfig, io IO) *Stages {
	return &Stages{
		live: live, io: io,
		c1: circuit{which: 1},
		c2: circuit{which: 2},
	}
}

// Start begins homing: both relief valves open, both direction valves
// forced off, for Config.Homing.Duration.
func (s *Stages) Start() {
	now := time.Now()
	s.mu.Lock()
	s.running = true
	s.homingStart = now
	s.lastSwitch = now
	for _, c := range []*circuit{&s.c1, &s.c2} {
		c.dir = DirDown
		c.phase = phaseHoming
		c.phaseStart = now
		c.history = nil
		c.filterBuf = nil
		c.lastFault = ""
	}
	s.mu.Unlock()

	_ = s.io.SetRelay(s.c1.cfg(s.live).DirRelay, false)
	_ = s.io.SetRelay(s.c2.cfg(s.live).DirRelay, false)
	_ = s.io.SetRelay(s.c1.cfg(s.live).ReliefRelay, true)
	_ = s.io.SetRelay(s.c2.cfg(s.live).ReliefRelay, true)
}

// Stop halts both circuits and de-energises all four relays.
func (s *Stages) Stop() {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()

	for _, c := range []*circuit{&s.c1, &s.c2} {
		cfg := c.cfg(s.live)
		_ = s.io.SetRelay(cfg.DirRelay, false)
		_ = s.io.SetRelay(cfg.ReliefRelay, false)
	}
}

func (s *Stages) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Tick should be called periodically (e.g. every Config.PollPeriod) by
// the controller's supervisory loop.
func (s *Stages) Tick(now time.Time) {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if !running {
		return
	}

	s.tickHoming(now)
	s.tickDriving(now, &s.c1)
	s.tickDriving(now, &s.c2)
	s.tickBarrier(now)
	s.tickReversing(now, &s.c1)
	s.tickReversing(now, &s.c2)
}

// tickHoming ends homing once Config.Homing.Duration has elapsed for
// both circuits, closing both relief valves to begin the first stroke.
func (s *Stages) tickHoming(now time.Time) {
	s.mu.Lock()
	if s.c1.phase != phaseHoming || s.c2.phase != phaseHoming {
		s.mu.Unlock()
		return
	}
	duration := s.live.Snapshot().Homing.Duration
	if now.Sub(s.homingStart) < duration {
		s.mu.Unlock()
		return
	}
	s.c1.phase, s.c2.phase = phaseDriving, phaseDriving
	s.c1.phaseStart, s.c2.phaseStart = now, now
	s.lastSwitch = now
	s.mu.Unlock()

	_ = s.io.SetRelay(s.c1.cfg(s.live).ReliefRelay, false)
	_ = s.io.SetRelay(s.c2.cfg(s.live).ReliefRelay, false)
}

// tickDriving watches one driving circuit for end-of-stroke (a pressure
// spike, or MaxStrokeTime elapsing) and moves it to the barrier once
// found, re-opening its relief valve.
func (s *Stages) tickDriving(now time.Time, c *circuit) {
	s.mu.Lock()
	if c.phase != phaseDriving {
		s.mu.Unlock()
		return
	}
	cfg := c.cfg(s.live)
	elapsed := now.Sub(c.phaseStart)
	s.mu.Unlock()

	raw, err := s.io.ReadPressureBar(cfg.HydPressureSensor)
	if err != nil {
		s.mu.Lock()
		c.lastFault = "hydraulic pressure sensor fault: " + err.Error()
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	p := raw
	if cfg.PressureFilterSamples > 1 {
		c.filterBuf = append(c.filterBuf, raw)
		if len(c.filterBuf) > cfg.PressureFilterSamples {
			c.filterBuf = c.filterBuf[len(c.filterBuf)-cfg.PressureFilterSamples:]
		}
		p = median(c.filterBuf)
	}
	c.history = append(c.history, pSample{now, p})
	cut := 0
	for i, h := range c.history {
		if now.Sub(h.t) <= cfg.SpikeWindow {
			cut = i
			break
		}
	}
	c.history = c.history[cut:]
	var spike bool
	if len(c.history) > 1 && elapsed >= cfg.MinStrokeTime && now.Sub(s.lastSwitch) >= s.live.Snapshot().PressureBlankTime {
		delta := p - c.history[0].p
		spike = delta >= cfg.SpikeDeltaBar
	}
	timedOut := elapsed >= cfg.MaxStrokeTime
	reachedBarrier := spike || timedOut
	if reachedBarrier {
		c.phase = phaseAtBarrier
		c.phaseStart = now
		s.lastSwitch = now
	}
	s.mu.Unlock()

	if reachedBarrier {
		_ = s.io.SetRelay(cfg.ReliefRelay, true)
	}
}

// tickBarrier fires both direction valves together, once both circuits
// have reached the barrier - never just one.
func (s *Stages) tickBarrier(now time.Time) {
	s.mu.Lock()
	if s.c1.phase != phaseAtBarrier || s.c2.phase != phaseAtBarrier {
		s.mu.Unlock()
		return
	}
	newDir := DirUp
	if s.c1.dir == DirUp {
		newDir = DirDown
	}
	s.c1.dir, s.c2.dir = newDir, newDir
	s.c1.strokeCount++
	s.c2.strokeCount++
	s.c1.phase, s.c2.phase = phaseReversing, phaseReversing
	s.c1.phaseStart, s.c2.phaseStart = now, now
	s.lastSwitch = now
	strokeCount := s.c1.strokeCount
	onReversal := s.OnReversal
	s.mu.Unlock()

	dirUp := newDir == DirUp
	_ = s.io.SetRelay(s.c1.cfg(s.live).DirRelay, dirUp)
	_ = s.io.SetRelay(s.c2.cfg(s.live).DirRelay, dirUp)

	if onReversal != nil {
		onReversal(newDir, strokeCount)
	}
}

// tickReversing closes one circuit's relief valve once ReverseDelay has
// elapsed since the direction valves flipped, starting its next stroke.
func (s *Stages) tickReversing(now time.Time, c *circuit) {
	s.mu.Lock()
	if c.phase != phaseReversing {
		s.mu.Unlock()
		return
	}
	cfg := c.cfg(s.live)
	ready := now.Sub(c.phaseStart) >= cfg.ReverseDelay
	if ready {
		c.phase = phaseDriving
		c.phaseStart = now
		c.history = nil
		c.filterBuf = nil
		s.lastSwitch = now
	}
	s.mu.Unlock()

	if ready {
		_ = s.io.SetRelay(cfg.ReliefRelay, false)
	}
}

type StageStatus struct {
	Name          string  `json:"name"`
	Running       bool    `json:"running"`
	Direction     string  `json:"direction"`
	StrokeCount   uint64  `json:"strokeCount"`
	HydPressure   float64 `json:"hydPressureBar"`
	StrokeElapsed float64 `json:"strokeElapsedSec"`
	Fault         string  `json:"fault,omitempty"`
}

func (s *Stages) Status() (s1, s2 StageStatus) {
	return s.circuitStatus(&s.c1), s.circuitStatus(&s.c2)
}

func (s *Stages) circuitStatus(c *circuit) StageStatus {
	cfg := c.cfg(s.live)

	s.mu.Lock()
	d := "DOWN"
	if c.dir == DirUp {
		d = "UP"
	}
	strokeCount := c.strokeCount
	phaseStart := c.phaseStart
	fault := c.lastFault
	running := s.running
	s.mu.Unlock()

	p, _ := s.io.ReadPressureBar(cfg.HydPressureSensor)
	return StageStatus{
		Name: cfg.Name, Running: running, Direction: d,
		StrokeCount: strokeCount, HydPressure: p,
		StrokeElapsed: time.Since(phaseStart).Seconds(),
		Fault:         fault,
	}
}
