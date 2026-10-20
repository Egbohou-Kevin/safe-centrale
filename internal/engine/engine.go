// Package engine is the alarm engine (see docs/engine.md). It is the domain core
// of the hub.
//
// It is deterministic: it does not read the clock, write anywhere or talk to any
// hardware. It is given inputs (signals, orders, ticks) that carry their own time,
// and returns actions for a driver to carry out. Every scenario can therefore be
// tested with a simulated clock, and a real sequence of events can be replayed.
//
// The package knows nothing about MQTT, JSON or the event contract: the wire
// subpackage translates between this domain and the contract. An architecture test
// (internal/archtest) enforces that boundary.
//
// An Engine is not safe for concurrent use: the driver must feed it one input at a
// time.
package engine

import (
	"fmt"
	"slices"
	"time"
)

// Engine is the alarm engine.
type Engine struct {
	cfg   Config
	newID func() string
	s     Snapshot
}

// New creates a disarmed engine. newID produces incident identifiers (for example
// contract.IDGenerator.New).
func New(cfg Config, newID func() string, now time.Time) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if newID == nil {
		return nil, fmt.Errorf("engine: newID is required")
	}
	e := &Engine{cfg: cfg, newID: newID, s: newSnapshot(now)}
	e.planSchedule(now)
	return e, nil
}

// Snapshot returns a copy of the current state.
func (e *Engine) Snapshot() Snapshot { return e.s.clone() }

// Restore replaces the state with a saved one. Call it before Boot.
func (e *Engine) Restore(s Snapshot) {
	s = s.clone()
	if s.Bypassed == nil {
		s.Bypassed = map[string]bool{}
	}
	if s.Open == nil {
		s.Open = map[string]bool{}
	}
	e.s = s
}

// Mode returns the current mode of the alarm.
func (e *Engine) Mode() Mode { return e.s.Mode }

// NextWake returns the time at which the driver must send a Tick, or the zero time
// if nothing is pending.
func (e *Engine) NextWake() time.Time { return e.s.Deadline }

// State returns the public summary of the engine at time now.
func (e *Engine) State(now time.Time) State {
	st := State{Mode: e.s.Mode, Since: e.s.Since}
	switch e.s.Mode {
	case ModeArming, ModeEntryDelay:
		st.EndsAt = e.s.Deadline
	case ModeTriggered:
		if inc := e.s.Incident; inc != nil {
			st.Cycle, st.Of = inc.Cycle, e.cfg.SirenCycles
		}
	}
	if now.Before(e.s.LockedUntil) {
		st.LockedUntil = e.s.LockedUntil
	}
	if e.s.Mode == ModeDisarmed && e.s.Schedule != nil {
		st.NextScheduled = e.s.Schedule.At
	}
	return st
}

// Handle processes an input and returns the actions to carry out. Deadlines that
// have passed are processed first, in order, then the input itself.
func (e *Engine) Handle(in Input) []Action {
	out := e.tick(in.at())
	switch v := in.(type) {
	case Signal:
		out = append(out, e.signal(v)...)
	case Command:
		out = append(out, e.command(v)...)
	}
	return out
}

// Boot resumes operation after a restart, once Restore has been called. current
// is the last known state of the devices (the retained state on the broker).
// Devices already in alarm trip the engine like an ordinary signal.
//
// Momentary sensors (motion) must therefore publish their return to rest, or a
// stale "active" state would trigger an alarm on restart.
func (e *Engine) Boot(now time.Time, current []Signal) []Action {
	out := e.tick(now)
	if e.s.Mode == ModeDisarmed && e.s.Schedule == nil {
		e.planSchedule(now) // a snapshot from before the schedule existed, or a new configuration
	}
	if e.s.Mode == ModeTriggered && e.s.Incident != nil && e.s.Incident.SirenOn {
		// A cycle replayed while the hub was off asked for a full cycle from its own
		// start: only what is left of it must sound now, and only once.
		out = slices.DeleteFunc(out, func(a Action) bool { _, on := a.(SirenOn); return on })
		out = append(out, SirenOn{Duration: e.s.Deadline.Sub(now)})
	}
	for _, sig := range current {
		sig.Now = now
		out = append(out, e.signal(sig)...)
	}
	return out
}

// tick processes the deadlines that have passed, each at its own time even if the
// tick is late.
func (e *Engine) tick(now time.Time) []Action {
	var out []Action
	for !e.s.Deadline.IsZero() && !now.Before(e.s.Deadline) {
		ref := e.s.Deadline
		switch e.s.Mode {
		case ModeArming:
			out = append(out, e.finishArming(ref)...)
		case ModeEntryDelay:
			cause := Cause{}
			if e.s.EntryCause != nil {
				cause = *e.s.EntryCause
			}
			out = append(out, e.trigger(ref, cause, AlertIntrusion)...)
		case ModeTriggered:
			out = append(out, e.sirenStep(ref)...)
		case ModeDisarmed:
			out = append(out, e.scheduleStep(ref)...)
		default:
			e.s.Deadline = time.Time{}
		}
	}
	return out
}

// setMode changes the mode. Entering disarmed plans the next automatic arming;
// leaving it drops the plan.
func (e *Engine) setMode(m Mode, at time.Time) {
	e.s.Mode = m
	e.s.Since = at
	if m == ModeDisarmed {
		e.planSchedule(at)
	} else {
		e.s.Schedule = nil
	}
}

// disarm goes back to disarmed from any mode: no incident, no delay, no bypass. The
// deadline becomes that of the next automatic arming.
func (e *Engine) disarm(at time.Time) {
	e.s.Incident, e.s.EntryCause = nil, nil
	e.s.Bypassed = map[string]bool{}
	e.setMode(ModeDisarmed, at)
}
