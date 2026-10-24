package escalation

import (
	"maps"
	"slices"
	"strings"
	"time"
)

// An alert can name the alarm under way it is part of (Raise.Alarm): a siren that
// does not sound during an alarm. Before anyone handles that alarm, the alert goes
// as any other. Once a contact acknowledged the alarm, or a person stopped it with a
// code, the alert is quiet: it still tells the fact to the same people, by the
// internet first, but never rings anyone. No call, urgency low, no further step,
// and the GSM only carries the fact to a phone that did not say it got it. A fire or
// panic still escalated keeps it loud: its people may count on that siren. See
// docs/escalation.md, "Sirene muette pendant une alarme".

// remembered is how long a handled alarm and a reported siren are kept.
const remembered = 24 * time.Hour

// handled records that an incident is handled. The first way it was handled is
// kept: a disarm after an acknowledgement does not overwrite it.
func (e *Escalation) handled(incident string, at time.Time, closed bool) {
	e.forget(at)
	if _, ok := e.s.Handled[incident]; ok || incident == "" {
		return
	}
	e.s.Handled[incident] = HandledAlarm{At: at, Closed: closed}
}

// forget drops what is older than a day from the handled alarms and the reported
// sirens.
func (e *Escalation) forget(now time.Time) {
	for id, h := range e.s.Handled {
		if now.Sub(h.At) >= remembered {
			delete(e.s.Handled, id)
		}
	}
	for key, at := range e.s.SirenReported {
		if now.Sub(at) >= remembered {
			delete(e.s.SirenReported, key)
		}
	}
}

// firstReport records a silent siren for its alarm, and reports whether it is the
// first time. The record is saved with the state, so a hub restarted during the
// alarm does not ring the phones again for the same siren.
func (e *Escalation) firstReport(r Raise) bool {
	key := r.Alarm + "/" + strings.Join(unionSorted(nil, r.Devices), ",")
	if _, ok := e.s.SirenReported[key]; ok {
		return false
	}
	e.s.SirenReported[key] = r.Now
	return true
}

// quietFor reports whether an alert of this alarm is told without ringing: the alarm
// is handled and no fire or panic is still escalated.
func (e *Escalation) quietFor(alarm string) bool {
	if alarm == "" {
		return false
	}
	_, handled := e.s.Handled[alarm]
	return handled && !e.lifeAlarmOpen()
}

// lifeAlarmOpen reports whether a fire or a panic of the site is still escalated:
// nobody answered it yet.
func (e *Escalation) lifeAlarmOpen() bool {
	for _, in := range e.s.Incidents {
		if in.Kind == KindFire || in.Kind == KindPanic {
			return true
		}
	}
	return false
}

// settle quietens the alerts under way whose alarm is now handled, after an
// acknowledgement or a close: that alarm's, or a fire's that kept them loud. Nothing
// else is touched: no incident is concluded for it, nothing already sent is taken
// back.
func (e *Escalation) settle() []Action {
	var out []Action
	for _, key := range slices.Sorted(maps.Keys(e.s.Incidents)) {
		in := e.s.Incidents[key]
		if in.Alarm == "" || in.Quiet || !e.quietFor(in.Alarm) {
			continue
		}
		quieten(&in)
		e.s.Incidents[key] = in
		out = append(out, e.finishIfDone(key)...)
	}
	return out
}

// quieten makes an alert under way quiet. Its next step is dropped once it told
// someone; its timers stay: the internet confirmation still brings the SMS of a
// message the cloud did not take, and the doubling becomes an SMS only, for a phone
// that did not say it got the message (see run). The call is withheld (see send).
func quieten(in *IncidentSnap) {
	in.Quiet = true
	if in.Step > 0 {
		in.NextAt = time.Time{}
	}
}

// roundDelivered reports whether a phone said it got an internet message of the
// contact's round: delivered, not only sent (the cloud handing it over).
func roundDelivered(in IncidentSnap, contact string, step int) bool {
	for _, d := range in.Deliveries {
		if d.Contact == contact && d.Step == step && d.Channel.internet() && d.Status == ResultDelivered {
			return true
		}
	}
	return false
}
