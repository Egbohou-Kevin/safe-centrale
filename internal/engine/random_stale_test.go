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
		ordered := r.bootKnown && sig.Now.Before(r.bootAt) && !r.apart[s] && !r.readBeforeStep(sig.Now)
		if !sig.Now.After(r.now) && r.states[s].Now.After(r.now) && !ordered {
			return true
		}
	}
	return false
}

// readBeforeStep tells whether a state dated at t may have been dated on the clock as
// it read at boot, before NTP set it forward, as Boot takes it (see bootRef.stepped):
// at the first Boot of a system run, in the trace's window (see traceWindow).
func (r *rig) readBeforeStep(t time.Time) bool {
	return r.firstOfRun && r.traceWindow(t)
}

// traceWindow tells whether t lies where the trace would date a state before a step at
// boot: since shown, before the system's start on the clock set right, at most the
// uptime after shown.
func (r *rig) traceWindow(t time.Time) bool {
	return r.bootKnown && !r.shown.IsZero() && r.shown.Before(r.bootAt) && !t.Before(r.shown) &&
		t.Before(r.bootAt) && t.Before(r.shown.Add(r.now.Sub(r.bootAt)))
}

// heardInTraceWindow tells whether a state the engine heard lies in the trace's window
// (see traceWindow): given again at a later Boot of the run, the trace would take it for
// news.
func (r *rig) heardInTraceWindow() bool {
	for _, sig := range r.states {
		if r.traceWindow(sig.Now) {
			return true
		}
	}
	return false
}

// staleStepped tells whether the broker gives back a state older than the one heard,
// that may have been dated before NTP set the clock forward (see readBeforeStep), the
// one heard not since the step: it cannot be told from a state dated while the hub was
// starting, which is news. The price accepted on 28 October 2026.
func (r *rig) staleStepped() bool {
	for s, sig := range r.retained {
		heard := r.states[s].Now
		if r.readBeforeStep(sig.Now) && (heard.Before(r.bootAt) || heard.After(r.now)) {
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
		since := !r.bootKnown || !sig.Now.Before(r.bootAt) || r.readBeforeStep(sig.Now)
		if !lasting(s.sensor) && s.sensor != SensorPanic && sig.Active && !sig.Now.After(r.now) && since {
			return true
		}
	}
	return false
}
