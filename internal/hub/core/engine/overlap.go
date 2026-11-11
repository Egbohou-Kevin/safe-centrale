package engine

import (
	"slices"
	"time"
)

// during handles an alarm raised while another one rings. Nothing of it is lost: it
// alerts under its own incident, once per kind and per ringing alarm (a tamper once
// per device, which signal sees to), unless it is the kind already ringing, which says
// it all. A fire during a fire says it all only while a detector of the ringing one is
// still in alarm (see joins).
//
// A fire, or a panic that sounds the siren, has no siren program of its own while the
// other one rings: it waits for it to end, then rings in turn (see waitFor and wake).
// An intrusion or a tamper while the site is armed under the alarm does not ring: the
// siren already does. Its escalation ends with the disarm that ends this arming, like
// an intrusion whose siren ran out.
func (e *Engine) during(at time.Time, cause Cause, kind AlertKind) []Action {
	inc := e.s.Incident
	if inc == nil {
		return nil
	}
	if kind == inc.Kind && kind != AlertTamper && (!waits(kind) || e.joins(cause)) {
		return nil
	}
	switch {
	case waits(kind):
		return e.waitFor(at, cause, kind)
	case kind == AlertTamper:
	case slices.Contains(e.s.Raised, kind):
		return nil
	default:
		e.s.Raised = append(e.s.Raised, kind)
	}
	al := e.alert(kind, &cause)
	if inc.ReturnTo != ModeDisarmed { // part of the arming, under way or done
		e.s.TimedOut = append(e.s.TimedOut, al.Incident)
	}
	return []Action{al}
}

// waits tells whether an alarm of this kind raised during another one waits for the
// siren: a fire, or a panic that sounds it (a silent panic never gets here).
func waits(kind AlertKind) bool { return kind == AlertFire || kind == AlertPanic }

// joins adds a device of the kind that rings to the alarm that rings, when one of its
// devices is still in alarm: the same fire, already told. Once all of them went back to
// rest, a device set off again (the same or another) is a new fire, which alerts again:
// the first alert may have been answered "only steam in the kitchen", and a silence
// must not take the siren from a fire in the stock room.
func (e *Engine) joins(cause Cause) bool {
	if len(e.s.Sounding) == 0 {
		return false
	}
	if !slices.Contains(e.s.Sounding, cause) {
		e.s.Sounding = append(e.s.Sounding, cause)
	}
	return true
}

// waitFor puts a fire (or a panic that sounds the siren) behind the alarm that rings,
// one entry per device still in alarm, so that the siren rings for it once that alarm
// ends as long as one of them is (see calm). The first device of its kind alerts under
// a new incident; another one while it waits joins that incident, and the same device
// again changes nothing. Once every device of the kind went back to rest, the kind
// waits no more: a device set off again after that is a new fire, which alerts again.
func (e *Engine) waitFor(at time.Time, cause Cause, kind AlertKind) []Action {
	if slices.ContainsFunc(e.s.Waiting, func(w Incident) bool { return w.Kind == kind && w.Cause == cause }) {
		return nil
	}
	if i := slices.IndexFunc(e.s.Waiting, func(w Incident) bool { return w.Kind == kind }); i >= 0 {
		joined := e.s.Waiting[i]
		joined.Cause = cause
		e.s.Waiting = append(e.s.Waiting, joined)
		return nil
	}
	al := e.alert(kind, &cause)
	e.s.Waiting = append(e.s.Waiting, Incident{ID: al.Incident, Kind: kind, Cause: cause, StartedAt: at})
	return []Action{al}
}

// wake starts the siren program of the first alarm that waited behind the one that
// just ended, whatever ended it (its cycles, a code, a disarm): a fire raised during
// an intrusion must not stay silent once the intrusion is over. It keeps the incident
// its alert was raised under, and does not alert again; the other devices waiting
// under that incident ring with it.
func (e *Engine) wake(at time.Time) []Action {
	if len(e.s.Waiting) == 0 || e.s.Mode == ModeTriggered {
		return nil
	}
	next := e.s.Waiting[0]
	var with []Cause
	for _, w := range e.s.Waiting {
		if w.ID == next.ID {
			with = append(with, w.Cause)
		}
	}
	e.dropWaiting(func(w Incident) bool { return w.ID == next.ID })
	out := e.ring(at, next)
	e.s.Sounding = with
	return out
}

// calm drops the alarms waiting for the siren whose device went back to rest: a smoke
// that cleared before the other alarm ended no longer needs the siren. The device no
// longer holds the fire that rings either (see joins).
func (e *Engine) calm(sig Signal) {
	same := func(c Cause) bool { return c.DeviceID == sig.DeviceID && c.Sensor == sig.Sensor }
	e.dropWaiting(func(w Incident) bool { return same(w.Cause) })
	e.dropSounding(same)
}

func (e *Engine) dropSounding(match func(Cause) bool) {
	e.s.Sounding = slices.DeleteFunc(slices.Clone(e.s.Sounding), match)
	if len(e.s.Sounding) == 0 {
		e.s.Sounding = nil
	}
}

func (e *Engine) dropWaiting(match func(Incident) bool) {
	e.s.Waiting = slices.DeleteFunc(slices.Clone(e.s.Waiting), match)
	if len(e.s.Waiting) == 0 {
		e.s.Waiting = nil
	}
}
