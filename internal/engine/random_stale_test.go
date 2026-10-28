package engine

import "time"

// What a restart of the random sequences cannot check, or checks only thanks to the
// boot time: the states the broker gives back older than the ones heard.

// staleBehind tells whether the broker gives back a state dated up to now while the
// one the engine heard is after now, and the boot time does not tell it from a state
// dated while the hub was starting: unknown, or the state dated since the system
// started, or the time heard set apart by a run before (see Engine.older).
func (r *rig) staleBehind() bool {
	for s, sig := range r.retained {
		ordered := r.bootKnown && sig.Now.Before(r.earliest()) && !r.apart[s]
		if !sig.Now.After(r.now) && r.states[s].Now.After(r.now) && !ordered {
			return true
		}
	}
	return false
}

// earliest is the earliest time the clock read since the system started, as Boot
// takes it: shown when before bootAt, bootAt otherwise.
func (r *rig) earliest() time.Time {
	if !r.shown.IsZero() && r.shown.Before(r.bootAt) {
		return r.shown
	}
	return r.bootAt
}

// staleStepped tells whether the broker gives back a state older than the one heard,
// dated since the time the clock read at boot, before the system's start on the clock
// set forward, the one heard before that start: it cannot be told from a state dated
// before NTP set the clock forward, which is news (see bootRef.stepped). The price
// accepted on 28 October 2026.
func (r *rig) staleStepped() bool {
	for s, sig := range r.retained {
		heard := r.states[s].Now
		if r.bootKnown && !sig.Now.Before(r.earliest()) && sig.Now.Before(r.bootAt) && heard.Before(r.bootAt) {
			return true
		}
	}
	return false
}

// staleCaughtUp tells whether the broker gives back a state older than one heard after
// a time the clock went back to, the clock since caught up with it.
func (r *rig) staleCaughtUp() bool {
	for s := range r.retained {
		if r.apart[s] && !r.states[s].Now.After(r.now) {
			return true
		}
	}
	return false
}

// keptBehind tells whether the broker gives back a state dated up to now while the one
// the engine heard is after now, whatever the boot time says.
func (r *rig) keptBehind() bool {
	for s, sig := range r.retained {
		if !sig.Now.After(r.now) && r.states[s].Now.After(r.now) {
			return true
		}
	}
	return false
}

func keepsTime(s SensorKind) bool { return lasting(s) || s == SensorPanic }

// momentaryUpToNow tells whether a momentary sensor is active in a state dated up to
// now, and not before the system started when Boot knows it: it cannot be told from a
// movement while the hub was starting.
func (r *rig) momentaryUpToNow() bool {
	for s, sig := range r.states {
		since := !r.bootKnown || !sig.Now.Before(r.earliest())
		if !lasting(s.sensor) && s.sensor != SensorPanic && sig.Active && !sig.Now.After(r.now) && since {
			return true
		}
	}
	return false
}
