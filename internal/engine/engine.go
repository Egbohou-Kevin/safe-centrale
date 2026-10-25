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
	// awaitingBoot is set by Restore and cleared by Boot: until then the engine asks
	// for no tick and replays no deadline, so that the deadlines missed while the hub
	// was off are replayed by Boot, once it knows the devices' current states, and the
	// siren resumes once.
	awaitingBoot bool
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
	if s.Tampered == nil {
		s.Tampered = map[string]bool{}
	}
	e.s = s
	e.awaitingBoot = true
}

// Mode returns the current mode of the alarm.
func (e *Engine) Mode() Mode { return e.s.Mode }

// NextWake returns the time at which the driver must send a Tick, or the zero time
// if nothing is pending. After Restore it is zero until Boot.
func (e *Engine) NextWake() time.Time {
	if e.awaitingBoot {
		return time.Time{}
	}
	return e.s.Deadline
}

// State returns the public summary of the engine at time now.
func (e *Engine) State(now time.Time) State {
	st := State{Mode: e.s.Mode, Since: e.s.Since}
	switch e.s.Mode {
	case ModeArming, ModeEntryDelay:
		st.EndsAt = e.s.Deadline
	case ModeTriggered:
		// The deadline is the siren schedule's own: saved with the snapshot and
		// replayed at its time by Boot, it stays right across a restart.
		if inc := e.s.Incident; inc != nil {
			st.Incident, st.SirenOn = inc.ID, inc.SirenOn
			st.Cycle, st.Of = inc.Cycle, e.cfg.SirenCycles
			st.EndsAt = e.s.Deadline
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
// have passed are processed first, in order, then the input itself. Between Restore
// and Boot the deadlines wait: Boot replays them once the devices' states are known.
func (e *Engine) Handle(in Input) []Action {
	e.pullBack(in.at())
	var out []Action
	if !e.awaitingBoot {
		out = e.tick(in.at())
	}
	switch v := in.(type) {
	case Signal:
		out = append(out, e.signal(v)...)
	case Command:
		out = append(out, e.command(v)...)
	case Forget:
		e.forget(v.DeviceID)
	}
	return out
}

// Boot resumes operation after a restart, once Restore has been called. current
// is the last known state of the devices (the retained state on the broker).
//
// The contacts' states are learned first, without acting on them, so that the
// deadlines missed while the hub was off are replayed against the doors as they are
// now: a door closed during the outage does not start the entry delay at the end of
// the exit delay. Then the devices still in alarm trip the engine like an ordinary
// signal, except a tamper already reported before the restart.
//
// Momentary sensors (motion) must therefore publish their return to rest, or a
// stale "active" state would trigger an alarm on restart.
func (e *Engine) Boot(now time.Time, current []Signal) []Action {
	e.awaitingBoot = false
	e.pullBack(now)
	alarms := e.learn(current)
	out := e.tick(now)
	if e.s.Mode == ModeDisarmed && e.s.Schedule == nil {
		e.planSchedule(now) // a snapshot from before the schedule existed, or a new configuration
	}
	if e.sirenSounds() && !slices.ContainsFunc(out, isSirenOn) {
		// The siren may have missed its order while the hub went down: it is told
		// again, for what is left of the cycle.
		out = append(out, SirenOn{Duration: e.s.Deadline.Sub(now)})
	}
	for _, sig := range alarms {
		sig.Now = now
		out = append(out, e.signal(sig)...)
	}
	return out
}

// pullBack brings back to now what a clock running ahead stamped in the future: a
// wrong code counts as entered now, and a lockout ends at most LockoutDuration from
// now. Otherwise, once the clock is pulled back (an RTC glitch, a bad time at boot
// corrected by NTP), they would hold until it caught up with them: a lockout an hour
// too long, or two forgotten mistakes and one real slip locking the site. Within a
// run the driver never hands an earlier time; a snapshot saved before a restart may
// still hold one.
func (e *Engine) pullBack(now time.Time) {
	for i, at := range e.s.Failures {
		if at.After(now) {
			e.s.Failures[i] = now
		}
	}
	if latest := now.Add(e.cfg.LockoutDuration); e.s.LockedUntil.After(latest) {
		e.s.LockedUntil = latest
	}
}

// learn records the devices' states given at Boot: which contacts are open, which
// tampers have ended. It returns the signals still to act on: every active one but
// the tampers already reported before the restart.
func (e *Engine) learn(current []Signal) []Signal {
	var alarms []Signal
	for _, sig := range current {
		if sig.Sensor == SensorTamper {
			if !sig.Active {
				delete(e.s.Tampered, sig.DeviceID)
				continue
			}
			if e.s.Tampered[sig.DeviceID] {
				continue
			}
		}
		e.trackContact(sig)
		if sig.Active {
			alarms = append(alarms, sig)
		}
	}
	return alarms
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
	return e.sirenFrom(now, out)
}

// sirenFrom keeps a single siren order among the actions of a tick: the last one,
// which gives the state the siren must be in now. A cycle that started before now
// (the tick is late: the hub was busy or off) sounds only for what is left of it.
// The driver's ticks always come a little late, so this is the usual case.
func (e *Engine) sirenFrom(now time.Time, out []Action) []Action {
	last := -1
	for i, a := range out {
		if isSirenOn(a) {
			last = i
		}
	}
	if last < 0 {
		return out
	}
	if e.sirenSounds() {
		out[last] = SirenOn{Duration: e.s.Deadline.Sub(now)}
	} else {
		last = -1 // the siren is off by now: its last order is a SirenOff
	}
	kept := out[:0]
	for i, a := range out {
		if !isSirenOn(a) || i == last {
			kept = append(kept, a)
		}
	}
	return kept
}

// sirenSounds tells whether the siren must be sounding: an alarm is on, in a cycle.
func (e *Engine) sirenSounds() bool {
	return e.s.Mode == ModeTriggered && e.s.Incident != nil && e.s.Incident.SirenOn
}

func isSirenOn(a Action) bool { _, on := a.(SirenOn); return on }

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
	e.s.Incident, e.s.EntryCause, e.s.TimedOut = nil, nil, nil
	e.s.Bypassed = map[string]bool{}
	e.setMode(ModeDisarmed, at)
}
