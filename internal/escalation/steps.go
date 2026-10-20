package escalation

import (
	"slices"
	"time"
)

func (e *Escalation) raise(r Raise) {
	if !slices.Contains(allKinds, r.Kind) {
		return
	}
	key := incidentKey(r.Incident, r.Kind)
	if in, ok := e.s.Incidents[key]; ok {
		// Another detector in the same incident: it is grouped, not told again.
		in.Devices = unionSorted(in.Devices, r.Devices)
		e.s.Incidents[key] = in
		return
	}
	e.s.Incidents[key] = IncidentSnap{
		ID: r.Incident, Kind: r.Kind, Urgency: r.Urgency,
		Devices: unionSorted(nil, r.Devices), Percent: r.Percent, RaisedAt: r.Now,
		NextAt: r.Now.Add(e.cfg.Plans[r.Kind].stepAt(0)),
		Rounds: map[string]Round{}, Deliveries: map[uint64]Delivery{},
	}
}

func unionSorted(a, b []string) []string {
	out := slices.Clone(a)
	for _, x := range b {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	slices.Sort(out)
	return out
}

// event is something that falls due: a step, an internet confirmation deadline or a
// GSM takeover.
type event struct {
	at      time.Time
	key     string
	contact string
	what    int // order at the same instant: steps first
}

const (
	evStep = iota
	evNet
	evGSM
)

// before orders events by time, then kind, then incident and contact, so that the
// replay never depends on the order of a map.
func (ev event) before(o event) bool {
	switch {
	case !ev.at.Equal(o.at):
		return ev.at.Before(o.at)
	case ev.what != o.what:
		return ev.what < o.what
	case ev.key != o.key:
		return ev.key < o.key
	}
	return ev.contact < o.contact
}

// advance runs everything due by now, in order, each at its own time, so a late
// tick (hub suspended, power cut) replays the schedule instead of skipping it.
func (e *Escalation) advance(now time.Time) []Action {
	var out []Action
	for {
		ev, ok := e.nextDue(now)
		if !ok {
			return out
		}
		in := e.s.Incidents[ev.key]
		out = append(out, e.run(&in, ev)...)
		e.s.Incidents[ev.key] = in
		out = append(out, e.finishIfDone(ev.key)...)
	}
}

// run carries out one event that fell due.
func (e *Escalation) run(in *IncidentSnap, ev event) []Action {
	if ev.what == evStep {
		return e.runStep(in, ev.at)
	}
	r := in.Rounds[ev.contact]
	useGSM := false
	switch ev.what {
	case evNet:
		r.NetDeadline = time.Time{}
		useGSM = !r.NetOK
	case evGSM:
		r.GSMDue = time.Time{}
		useGSM = !e.plan(*in).InternetOnly || !r.NetOK
	}
	in.Rounds[ev.contact] = r
	if !useGSM {
		return nil
	}
	return e.gsm(in, ev.contact, ev.at)
}

// nextDue returns the earliest event due at or before now.
func (e *Escalation) nextDue(now time.Time) (event, bool) {
	var best event
	found := false
	consider := func(ev event) {
		if ev.at.IsZero() || ev.at.After(now) {
			return
		}
		if !found || ev.before(best) {
			best, found = ev, true
		}
	}
	for key, in := range e.s.Incidents {
		consider(event{at: in.NextAt, key: key, what: evStep})
		for c, r := range in.Rounds {
			consider(event{at: r.NetDeadline, key: key, contact: c, what: evNet})
			consider(event{at: r.GSMDue, key: key, contact: c, what: evGSM})
		}
	}
	return best, found
}

// runStep tells the contacts of the next step. A step nobody is left for is skipped
// at once rather than waited for.
func (e *Escalation) runStep(in *IncidentSnap, at time.Time) []Action {
	plan := e.plan(*in)
	n := plan.totalSteps()
	for in.Step < n {
		step := in.Step
		in.Step++
		in.NextAt = time.Time{}
		if in.Step < n {
			in.NextAt = in.RaisedAt.Add(plan.stepAt(in.Step))
		}
		wave := plan.step(step)
		contacts := e.contactsOf(wave.To)
		if len(contacts) == 0 {
			continue
		}
		var out []Action
		in.WaveContacts = in.WaveContacts[:0]
		for _, ct := range contacts {
			in.WaveContacts = append(in.WaveContacts, ct.ID)
			out = append(out, e.notify(in, ct, step, wave, at)...)
		}
		return out
	}
	return nil
}

// contactsOf lists the contacts of several audiences once each, in configuration
// order.
func (e *Escalation) contactsOf(to []Audience) []Contact {
	picked := map[string]bool{}
	for _, a := range to {
		for _, ct := range e.cfg.audience(a) {
			picked[ct.ID] = true
		}
	}
	var out []Contact
	for _, ct := range e.cfg.Contacts {
		if picked[ct.ID] {
			out = append(out, ct)
		}
	}
	return out
}

// finishIfDone concludes an incident once no step and no timer is left.
func (e *Escalation) finishIfDone(key string) []Action {
	in, ok := e.s.Incidents[key]
	if !ok || !in.NextAt.IsZero() {
		return nil
	}
	for _, r := range in.Rounds {
		// A message held back for the uplink is still owed.
		if !r.NetDeadline.IsZero() || !r.GSMDue.IsZero() || r.NetHeld {
			return nil
		}
	}
	return e.conclude(key, OutcomeUnanswered, "")
}

func (e *Escalation) conclude(key string, outcome Outcome, by string) []Action {
	in, ok := e.s.Incidents[key]
	if !ok {
		return nil
	}
	delete(e.s.Incidents, key)
	return []Action{Concluded{Incident: in.ID, Kind: in.Kind, Outcome: outcome, By: by}}
}
