package main

import (
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

// Stage drives one hydraulic ram (and its pair of gas cylinders) back and
// forth between its two direction-valve states. Each tick it decides
// whether the current stroke has ended: either the configured max stroke
// time has elapsed, or hydraulic pressure has spiked (the ram stalling
// against the gas column is the normal end-of-stroke signature for a
// booster). On reversal it flips the direction relay and pulses the
// secondary relay for PulseDuration, coincident with the direction
// change, as specified.
//
// Its tuning (stroke times, spike threshold, pulse duration) is read
// fresh from LiveConfig on every tick, so changes made from the admin
// web screen take effect on the next tick - no restart needed.
type Stage struct {
	live  *LiveConfig
	which int // 1 -> Stage1 fields, 2 -> Stage2 fields
	io    IO

	mu          sync.Mutex
	running     bool
	dir         Direction
	strokeStart time.Time
	strokeCount uint64
	history     []pSample
	lastFault   string

	// AllowReverse lets an external synchronisation supervisor briefly
	// hold a stage at the end of its stroke (direction valve left as-is)
	// until the other stage catches up. Return true to permit the
	// reversal now. May be left nil (always allowed).
	AllowReverse func(s *Stage) bool
	// OnReversal fires (outside the lock) right after a reversal is
	// issued - used for telemetry/logging.
	OnReversal func(s *Stage)
}

func NewStage(live *LiveConfig, which int, io IO) *Stage {
	return &Stage{live: live, which: which, io: io, dir: DirDown}
}

// cfg reads this stage's current tuning from the live config.
func (s *Stage) cfg() StageConfig {
	c := s.live.Snapshot()
	if s.which == 1 {
		return c.Stage1
	}
	return c.Stage2
}

// Start begins reciprocating from a stopped state, continuing in whatever
// direction the ram was last left in.
func (s *Stage) Start() {
	s.mu.Lock()
	s.running = true
	s.strokeStart = time.Now()
	s.history = nil
	s.lastFault = ""
	dirUp := s.dir == DirUp
	s.mu.Unlock()
	_ = s.io.SetRelay(s.cfg().DirRelay, dirUp)
}

// Stop halts reciprocation and de-energises both relays for this stage.
func (s *Stage) Stop() {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
	cfg := s.cfg()
	_ = s.io.SetRelay(cfg.DirRelay, false)
	_ = s.io.SetRelay(cfg.PulseRelay, false)
}

func (s *Stage) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Stage) StrokeCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.strokeCount
}

// Tick should be called periodically (e.g. every Config.PollPeriod) by
// the controller's supervisory loop.
func (s *Stage) Tick(now time.Time) {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	elapsed := now.Sub(s.strokeStart)
	s.mu.Unlock()

	cfg := s.cfg()

	p, err := s.io.ReadPressureBar(cfg.HydPressureSensor)
	if err != nil {
		s.mu.Lock()
		s.lastFault = "hydraulic pressure sensor fault: " + err.Error()
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	s.history = append(s.history, pSample{now, p})
	cut := 0
	for i, h := range s.history {
		if now.Sub(h.t) <= cfg.SpikeWindow {
			cut = i
			break
		}
	}
	s.history = s.history[cut:]
	var spike bool
	if len(s.history) > 1 && elapsed >= cfg.MinStrokeTime {
		delta := p - s.history[0].p
		spike = delta >= cfg.SpikeDeltaBar
	}
	timedOut := elapsed >= cfg.MaxStrokeTime
	s.mu.Unlock()

	if !spike && !timedOut {
		return
	}
	if s.AllowReverse != nil && !s.AllowReverse(s) {
		return // held for synchronisation; re-evaluated next tick
	}
	s.reverse(now, cfg)
}

func (s *Stage) reverse(now time.Time, cfg StageConfig) {
	s.mu.Lock()
	if s.dir == DirDown {
		s.dir = DirUp
	} else {
		s.dir = DirDown
	}
	dirUp := s.dir == DirUp
	s.strokeStart = now
	s.strokeCount++
	s.history = nil
	s.mu.Unlock()

	_ = s.io.SetRelay(cfg.DirRelay, dirUp)
	go s.pulseSecondary(cfg)

	if s.OnReversal != nil {
		s.OnReversal(s)
	}
}

// pulseSecondary energises the secondary valve for the configured
// duration (~500ms by default), coincident with the direction change.
// The pulse duration used is the one in effect at the moment of
// reversal; a config change mid-pulse does not retroactively change an
// already-started pulse.
func (s *Stage) pulseSecondary(cfg StageConfig) {
	_ = s.io.SetRelay(cfg.PulseRelay, true)
	time.Sleep(cfg.PulseDuration)
	_ = s.io.SetRelay(cfg.PulseRelay, false)
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

func (s *Stage) Status() StageStatus {
	cfg := s.cfg()

	s.mu.Lock()
	d := "DOWN"
	if s.dir == DirUp {
		d = "UP"
	}
	running := s.running
	strokeCount := s.strokeCount
	strokeStart := s.strokeStart
	fault := s.lastFault
	s.mu.Unlock()

	p, _ := s.io.ReadPressureBar(cfg.HydPressureSensor)
	return StageStatus{
		Name: cfg.Name, Running: running, Direction: d,
		StrokeCount: strokeCount, HydPressure: p,
		StrokeElapsed: time.Since(strokeStart).Seconds(),
		Fault:         fault,
	}
}
