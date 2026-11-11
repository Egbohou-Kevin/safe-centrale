package escalation

import (
	"maps"
	"slices"
	"time"
)

// result records a channel's report about one message. Only a report about the
// contact's current round moves its timers, and a confirmation that stops the last
// timer ends the escalation.
func (e *Escalation) result(r Result) []Action {
	key, msg, ok := e.delivery(r)
	if !ok {
		return nil
	}
	in := e.s.Incidents[key]
	d := in.Deliveries[msg]
	d.Status = r.Status
	in.Deliveries[msg] = d
	round, current := in.Rounds[d.Contact]
	if current && round.Step == d.Step {
		applyResult(in, d, &round, r)
		in.Rounds[d.Contact] = round
		pullForwardIfAllUnreachable(&in, r.Now)
	}
	e.s.Incidents[key] = in
	return e.finishIfDone(key)
}

// delivery finds the message a result is about: the incident's key and the
// message's key.
func (e *Escalation) delivery(r Result) (string, uint64, bool) {
	for key, in := range e.s.Incidents {
		if r.Ref == "" {
			if _, ok := in.Deliveries[r.Key]; ok {
				return key, r.Key, true
			}
			continue
		}
		for msg, d := range in.Deliveries {
			if d.Ref == r.Ref {
				return key, msg, true
			}
		}
	}
	return "", 0, false
}

func applyResult(in IncidentSnap, d Delivery, round *Round, r Result) {
	if !d.Channel.internet() {
		return
	}
	switch r.Status {
	case ResultSent, ResultDelivered:
		round.NetOK = true
		round.NetDeadline = time.Time{}
	case ResultFailed:
		// Every internet message of the round failed: the GSM channels do not wait
		// for the deadline. Without a deadline running, nothing waits for them: an
		// internet-only round must stay off the GSM, and in any other round the GSM
		// channels already went.
		if !round.NetDeadline.IsZero() && !round.NetOK && !roundHasPending(in, d.Contact, round.Step, true) {
			round.NetDeadline = r.Now
		}
	}
}

// roundHasPending reports whether a message of the round has no outcome yet
// (internetOnly restricts the check to the internet channels).
func roundHasPending(in IncidentSnap, contact string, step int, internetOnly bool) bool {
	for _, d := range in.Deliveries {
		if d.Contact != contact || d.Step != step || (internetOnly && !d.Channel.internet()) {
			continue
		}
		if d.Status == "" || d.Status == ResultQueued {
			return true
		}
	}
	return false
}

// roundAllFailedOn reports whether the round sent something and every message of it
// failed (internetOnly restricts the check to the internet channels).
func roundAllFailedOn(in IncidentSnap, contact string, step int, internetOnly bool) bool {
	n := 0
	for _, d := range in.Deliveries {
		if d.Contact != contact || d.Step != step || (internetOnly && !d.Channel.internet()) {
			continue
		}
		if d.Status != ResultFailed {
			return false
		}
		n++
	}
	return n > 0
}

// pullForwardIfAllUnreachable: when no one the last step told can be reached, the
// next step does not wait for its delay. An internet message held while the uplink is
// down does not hold the wave: the GSM was the only path, and every message of it
// failed. The held one is still owed, and goes when the uplink returns (retryInternet).
func pullForwardIfAllUnreachable(in *IncidentSnap, now time.Time) {
	if in.NextAt.IsZero() || !in.NextAt.After(now) || len(in.WaveContacts) == 0 {
		return
	}
	for _, c := range in.WaveContacts {
		r, ok := in.Rounds[c]
		if !ok || r.NetOK || !r.NetDeadline.IsZero() || !r.GSMDue.IsZero() ||
			roundHasPending(*in, c, r.Step, false) || !roundAllFailedOn(*in, c, r.Step, false) {
			return
		}
	}
	in.NextAt = now
}

// ack ends the escalation of an incident a known contact answered, and remembers
// that it is handled, even when its escalation was already over (a late "1"): an
// alert that names it as its alarm is then told without ringing.
func (e *Escalation) ack(a Ack) []Action {
	if _, ok := e.cfg.contact(a.Contact); !ok {
		return nil
	}
	e.handled(a.Incident, a.Now, false)
	var out []Action
	for _, key := range e.keysOf(a.Incident) {
		out = append(out, e.conclude(key, OutcomeAcknowledged, a.Contact)...)
	}
	return append(out, e.settle()...)
}

// close ends the escalation of an incident a person stopped with a code, which
// handles it too.
func (e *Escalation) close(c Close) []Action {
	e.handled(c.Incident, c.Now, true)
	var out []Action
	for _, key := range e.keysOf(c.Incident) {
		out = append(out, e.conclude(key, OutcomeClosed, "")...)
	}
	return append(out, e.settle()...)
}

// keysOf returns the keys of every kind of alert under an incident, sorted.
func (e *Escalation) keysOf(incident string) []string {
	var keys []string
	for k, in := range e.s.Incidents {
		if in.ID == incident {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// uplink records the link to the cloud. When it comes back, what the internet
// channels could not carry goes out.
func (e *Escalation) uplink(u Uplink) []Action {
	e.s.UplinkDown = !u.Up
	if !u.Up {
		return nil
	}
	var out []Action
	for _, key := range slices.Sorted(maps.Keys(e.s.Incidents)) {
		in := e.s.Incidents[key]
		for _, id := range slices.Sorted(maps.Keys(in.Rounds)) {
			out = append(out, e.retryInternet(&in, id, u.Now)...)
		}
		e.s.Incidents[key] = in
		out = append(out, e.finishIfDone(key)...) // a held message that went may have been the last one owed
	}
	return out
}

// retryInternet sends again, to one contact, the internet messages of the current
// round that were held back or all failed.
func (e *Escalation) retryInternet(in *IncidentSnap, contact string, now time.Time) []Action {
	round := in.Rounds[contact]
	ct, ok := e.cfg.contact(contact)
	if !ok || round.NetOK {
		return nil
	}
	internet, _ := e.split(ct)
	failed := !roundHasPending(*in, contact, round.Step, true) && roundAllFailedOn(*in, contact, round.Step, true)
	if len(internet) == 0 || !(round.NetHeld || failed) {
		return nil
	}
	round.NetHeld = false
	var out []Action
	for _, ch := range internet {
		out = append(out, e.resend(in, ct, ch, &round, now)...)
	}
	if !e.plan(*in).InternetOnly {
		round.NetDeadline = now.Add(e.cfg.InternetConfirm)
	}
	in.Rounds[contact] = round
	return out
}
