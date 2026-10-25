package engine

import (
	"slices"
	"time"
)

// during handles an alarm raised while another one rings. Nothing of it is lost: it
// alerts under its own incident, once per kind and per ringing alarm (a tamper once
// per device, which signal sees to), unless it is the kind already ringing, which says
// it all.
//
// A fire, or a panic that sounds the siren, has no siren program of its own while the
// other one rings: it waits for it to end, then rings in turn (see wake). An intrusion
// or a tamper while the site is armed under the alarm does not ring: the siren already
// does. Its escalation ends with the disarm that ends this arming, like an intrusion
// whose siren ran out.
func (e *Engine) during(at time.Time, cause Cause, kind AlertKind) []Action {
	inc := e.s.Incident
	if inc == nil {
		return nil
	}
	if kind != AlertTamper && (kind == inc.Kind || slices.Contains(e.s.Raised, kind) || e.waiting(kind)) {
		return nil
	}
	al := e.alert(kind, &cause)
	if kind != AlertTamper {
		e.s.Raised = append(e.s.Raised, kind)
	}
	switch {
	case kind == AlertFire || kind == AlertPanic:
		e.s.Waiting = append(e.s.Waiting, Incident{ID: al.Incident, Kind: kind, Cause: cause, StartedAt: at})
	case inc.ReturnTo == ModeArmed:
		e.s.TimedOut = append(e.s.TimedOut, al.Incident)
	}
	return []Action{al}
}

func (e *Engine) waiting(kind AlertKind) bool {
	return slices.ContainsFunc(e.s.Waiting, func(w Incident) bool { return w.Kind == kind })
}

// wake starts the siren program of the first alarm that waited behind the one that
// just ended, whatever ended it (its cycles, a code, a disarm): a fire raised during
// an intrusion must not stay silent once the intrusion is over. It keeps the incident
// its alert was raised under, and does not alert again.
func (e *Engine) wake(at time.Time) []Action {
	if len(e.s.Waiting) == 0 || e.s.Mode == ModeTriggered {
		return nil
	}
	next := e.s.Waiting[0]
	e.s.Waiting = slices.Clone(e.s.Waiting[1:])
	if len(e.s.Waiting) == 0 {
		e.s.Waiting = nil
	}
	return e.ring(at, next)
}

// calm drops the alarms waiting for the siren whose device went back to rest: a smoke
// that cleared before the other alarm ended no longer needs the siren.
func (e *Engine) calm(sig Signal) {
	e.dropWaiting(func(c Cause) bool { return c.DeviceID == sig.DeviceID && c.Sensor == sig.Sensor })
}

func (e *Engine) dropWaiting(match func(Cause) bool) {
	e.s.Waiting = slices.DeleteFunc(e.s.Waiting, func(w Incident) bool { return match(w.Cause) })
	if len(e.s.Waiting) == 0 {
		e.s.Waiting = nil
	}
}
