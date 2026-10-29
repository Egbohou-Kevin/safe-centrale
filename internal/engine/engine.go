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
	// seen is the latest time the engine was handed: a time before it means the clock
	// was corrected backwards (see pullBack).
	seen time.Time
	// early are the sensors ("device/sensor") whose state changed between Restore and
	// Boot: applied to the saved state, that change may have been dropped there, and
	// Boot must take it for this run's news (see learn).
	early map[string]bool
	// apartBefore are the sensors whose time was set apart (HeardAhead) when the state
	// was restored: set apart by a run before, their time is on a clock older than the
	// one the broker's states were dated on since, which the boot time cannot order
	// (see older). Boot drops it.
	apartBefore map[string]bool
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
	if s.Mode == ModeArmed && s.ArmedSince.IsZero() {
		s.ArmedSince = s.Since // saved before ArmedSince was kept
	}
	e.s = s
	e.awaitingBoot = true
	e.apartBefore = map[string]bool{}
	for key := range s.HeardAhead {
		e.apartBefore[key] = true
	}
}

// Mode returns the current mode of the alarm.
func (e *Engine) Mode() Mode { return e.s.Mode }

// NextWake returns the time at which the driver must send a Tick, or the zero time
// if nothing is pending. After Restore it is zero until Boot.
func (e *Engine) NextWake() time.Time {
	if e.awaitingBoot {
		return time.Time{}
	}
	return e.due()
}

// State returns the public summary of the engine at time now.
func (e *Engine) State(now time.Time) State {
	// The mode's start as it was taken, not where a clock set back brought it.
	st := State{Mode: e.s.Mode, Since: later(e.s.Since, e.s.SinceAhead), ExitDelay: e.cfg.ExitDelay}
	switch e.s.Mode {
	case ModeArmed:
		// Armed since the arming, through the alarms that left the site armed (trial
		// of 26 October 2026: "armed since 10:33" for an arming at 10:30). One later
		// than the mode's own start comes from a clock set back since: not said.
		if a := e.s.ArmedSince; !a.IsZero() && a.Before(st.Since) {
			st.Since = a
		}
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
	if now.Before(e.s.KeypadLockedUntil) {
		st.KeypadLockedUntil = e.s.KeypadLockedUntil
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
	ref := ""
	switch v := in.(type) {
	case Signal:
		out = append(out, e.signal(v)...)
	case Command:
		ref = v.ID
		out = append(out, e.command(v)...)
	case Forget:
		e.forget(v.DeviceID)
	case CodeEntered:
		e.codeEntered(v)
	}
	if _, typed := in.(CodeEntered); !typed {
		e.checked(ref, in.at())
	}
	return out
}

// Stepped takes a forward step d of the system clock, which the driver sees on the
// monotonic clock (driver.Core.Stepped): a Pi without a battery clock starts from the
// last time it knew, hours late after a power cut, and NTP sets it forward once
// internet is there. What the engine times moves with the clock, so that each keeps the
// time that really passed: the exit or entry delay, the siren cycle or pause, an entry
// delay an alarm interrupted, the codes being checked, both lockouts and the wrong codes
// of their windows, and the start of the incident (its duration). Otherwise an exit delay
// would arm at once, an intrusion during an entry delay would ring at once and a siren
// would stop. The automatic arming is a time of day, not a wait: it stays, and so do
// the times the engine only says (since, armed since, the last alarm) or compares with
// a device's (Heard, HeardAhead: the next input, at the clock as it is after the step,
// puts back what it caught up with, see pullBack).
func (e *Engine) Stepped(d time.Duration) {
	if d <= 0 {
		return
	}
	if e.s.Mode != ModeDisarmed { // disarmed, the deadline is the automatic arming's
		e.s.Deadline = moved(e.s.Deadline, d)
	}
	e.s.EntryEnds, e.s.LockedUntil = moved(e.s.EntryEnds, d), moved(e.s.LockedUntil, d)
	e.s.KeypadLockedUntil = moved(e.s.KeypadLockedUntil, d)
	for _, failures := range [][]time.Time{e.s.Failures, e.s.KeypadFailures} {
		for i, at := range failures {
			failures[i] = moved(at, d)
		}
	}
	for ref, at := range e.s.Checking {
		e.s.Checking[ref] = moved(at, d)
	}
	if inc := e.s.Incident; inc != nil {
		inc.StartedAt = moved(inc.StartedAt, d)
	}
	for i := range e.s.Waiting {
		e.s.Waiting[i].StartedAt = moved(e.s.Waiting[i].StartedAt, d)
	}
}

// moved is t moved by d, the zero time staying zero (no deadline).
func moved(t time.Time, d time.Duration) time.Time {
	if t.IsZero() {
		return t
	}
	return t.Add(d)
}

// step is the length of what the current deadline ends: the exit or the entry delay,
// a siren cycle or the pause after it; zero while disarmed, whose deadline is the
// automatic arming.
func (e *Engine) step() time.Duration {
	switch e.s.Mode {
	case ModeArming:
		return e.cfg.ExitDelay
	case ModeEntryDelay:
		return e.cfg.EntryDelay
	case ModeTriggered:
		if e.s.Incident != nil && !e.s.Incident.SirenOn {
			return e.cfg.SirenPause
		}
		return e.cfg.SirenCycle
	}
	return 0
}

// tick processes the deadlines that have passed, each at its own time even if the
// tick is late.
func (e *Engine) tick(now time.Time) []Action {
	var out []Action
	for ref := e.due(); !ref.IsZero() && !now.Before(ref); ref = e.due() {
		if late := e.lateEntryDue(); !late.IsZero() && ref.Equal(late) {
			out = append(out, e.lateEntry(ref)...)
			continue
		}
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
			out = append(out, e.scheduleStep(ref, now)...)
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
	e.s.Since, e.s.SinceAhead = at, time.Time{}
	if m == ModeDisarmed || m == ModeArming {
		e.s.ArmedSince = time.Time{}
	}
	if m == ModeDisarmed {
		e.planSchedule(at)
	} else {
		e.s.Schedule = nil
	}
}

// disarm goes back to disarmed from any mode: no incident, no delay, no bypass. The
// deadline becomes that of the next automatic arming.
func (e *Engine) disarm(at time.Time) {
	e.s.Incident, e.s.EntryCause, e.s.TimedOut, e.s.Raised, e.s.Sounding = nil, nil, nil, nil, nil
	e.s.EntryEnds = time.Time{}
	e.s.Bypassed = map[string]bool{}
	e.setMode(ModeDisarmed, at)
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
