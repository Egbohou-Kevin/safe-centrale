package engine

import (
	"maps"
	"strings"
	"time"
)

// signal applies a sensor change. A device's tamper sensor always means tamper,
// whatever its zone and even without one; any other sensor counts only if the
// device's zone reacts to it. Contacts are tracked open or closed in every mode, so
// that arming knows which doors are open.
func (e *Engine) signal(sig Signal) []Action {
	zone, known := e.cfg.Zones[sig.DeviceID]
	if sig.Sensor == SensorTamper {
		e.hear(sig)
		if !sig.Active {
			delete(e.s.Tampered, sig.DeviceID)
			return nil
		}
		if e.s.Tampered[sig.DeviceID] {
			return nil // already reported, and not over since
		}
		e.s.Tampered[sig.DeviceID] = true
		return e.tamper(sig.Now, Cause{DeviceID: sig.DeviceID, Sensor: sig.Sensor, Zone: zone.Name})
	}
	if !known || !Accepts(zone.Kind, sig.Sensor) {
		return nil
	}
	e.hear(sig)
	e.trackContact(sig)
	if !sig.Active {
		e.calm(sig)
		return nil
	}
	cause := Cause{DeviceID: sig.DeviceID, Sensor: sig.Sensor, Zone: zone.Name}

	switch zone.Kind {
	case ZoneFire:
		return e.trigger(sig.Now, cause, AlertFire)

	case ZonePanic:
		if e.cfg.PanicSiren {
			return e.trigger(sig.Now, cause, AlertPanic)
		}
		return []Action{e.alert(AlertPanic, &cause)} // silent alert

	case ZoneEntry, ZoneInstant, ZoneInterior:
		if e.s.Bypassed[sig.DeviceID] {
			return nil
		}
		return e.armedZone(sig.Now, zone.Kind, cause)
	}
	return nil
}

// trackContact records whether a door or window is open, for the contacts of a zone
// that reacts to them; other signals change nothing.
func (e *Engine) trackContact(sig Signal) {
	if sig.Sensor != SensorContact || !Accepts(e.cfg.Zones[sig.DeviceID].Kind, SensorContact) {
		return
	}
	if sig.Active {
		e.s.Open[sig.DeviceID] = true
	} else {
		delete(e.s.Open, sig.DeviceID)
	}
}

// tamper applies the rule of the 24h_tamper zones: an alert without siren while
// disarmed, an alarm otherwise. A tamper never changes the disarmed mode, so it never
// ends the installation mode either.
func (e *Engine) tamper(at time.Time, cause Cause) []Action {
	if e.s.Mode == ModeDisarmed {
		return []Action{e.alert(AlertTamper, &cause)}
	}
	return e.trigger(at, cause, AlertTamper)
}

// forget drops a device removed from the site: it no longer holds a door open, a
// bypass, or an alarm waiting for the siren. What the incidents remember of it stays.
func (e *Engine) forget(id string) {
	delete(e.s.Open, id)
	delete(e.s.Bypassed, id)
	delete(e.s.Tampered, id)
	ofDevice := func(key string, _ time.Time) bool { return strings.HasPrefix(key, id+"/") }
	maps.DeleteFunc(e.s.Heard, ofDevice)
	maps.DeleteFunc(e.s.HeardAhead, ofDevice)
	maps.DeleteFunc(e.s.SetAside, ofDevice)
	maps.DeleteFunc(e.s.Moving, ofDevice)
	e.dropWaiting(func(w Incident) bool { return w.Cause.DeviceID == id })
	e.dropSounding(func(c Cause) bool { return c.DeviceID == id })
}

// armedZone applies the rules of the zones that only watch while the system is
// armed.
func (e *Engine) armedZone(at time.Time, kind ZoneKind, cause Cause) []Action {
	switch e.s.Mode {
	case ModeArmed:
		if kind == ZoneEntry {
			e.setMode(ModeEntryDelay, at)
			e.s.EntryCause = &cause
			e.s.Deadline = at.Add(e.cfg.EntryDelay)
			return []Action{Emit{EntryDelayStarted{DeviceID: cause.DeviceID, Zone: cause.Zone, EndsAt: e.s.Deadline}}}
		}
		return e.trigger(at, cause, AlertIntrusion)

	case ModeEntryDelay, ModeTriggered:
		// A window during the entry delay gets no grace; nor does one during another
		// alarm while the site is armed under it (see during).
		if kind == ZoneInstant && (e.s.Mode == ModeEntryDelay || e.armedUnder()) {
			return e.trigger(at, cause, AlertIntrusion)
		}
		// Motion from the person coming in is expected, and a second opening of the
		// entry door does not restart the delay. The same goes during an alarm: whoever
		// comes in is most often answering it.
	}
	return nil // disarmed, exit delay, or already ringing for an intrusion
}

// armedUnder tells whether the site is armed under the alarm that rings: it goes back
// to armed once that alarm ends.
func (e *Engine) armedUnder() bool {
	return e.s.Mode == ModeTriggered && e.s.Incident != nil && e.s.Incident.ReturnTo == ModeArmed
}

// trigger sets the alarm off. If an alarm is already ringing, see during.
func (e *Engine) trigger(at time.Time, cause Cause, kind AlertKind) []Action {
	if e.s.Mode == ModeTriggered {
		return e.during(at, cause, kind)
	}
	inc := Incident{ID: e.newID(), Kind: kind, Cause: cause, StartedAt: at}
	return append(e.ring(at, inc), Alert{Incident: inc.ID, Kind: kind, Urgency: urgency(kind), Cause: &cause})
}

// ring starts the siren program of an alarm: it rings from at, for its cycles, and
// the system goes back to the mode it was in once they are over (see endIncident). An
// alarm of another kind than an intrusion during an entry delay does not drop it: the
// delay goes on behind the alarm, and becomes an intrusion if no code ends it (see
// lateEntry). The automatic arming planned, while disarmed or during the exit delay it
// started, is kept for the end of the alarm.
func (e *Engine) ring(at time.Time, inc Incident) []Action {
	inc.ReturnTo = ModeArmed
	if e.s.Mode == ModeDisarmed || e.s.Mode == ModeArming {
		inc.ReturnTo = e.s.Mode
	}
	if e.s.Mode == ModeEntryDelay && inc.Kind != AlertIntrusion {
		e.s.EntryEnds = e.s.Deadline
	} else {
		e.s.EntryCause = nil
	}
	inc.Cycle, inc.SirenOn = 1, true
	sch := e.s.Schedule
	e.setMode(ModeTriggered, at)
	e.s.Schedule = sch
	e.s.Raised, e.s.Sounding = nil, nil
	if waits(inc.Kind) {
		e.s.Sounding = []Cause{inc.Cause}
	}
	e.s.Incident = &inc
	e.s.Deadline = at.Add(e.cfg.SirenCycle)
	last := inc.Cause
	e.s.LastTrigger, e.s.LastTriggerAt = &last, at
	return []Action{
		Emit{Triggered{Incident: inc.ID, Cause: inc.Cause}},
		Emit{SirenCycleStarted{Incident: inc.ID, Cycle: 1, Of: e.cfg.SirenCycles}},
		SirenOn{Duration: e.cfg.SirenCycle},
	}
}

// sirenStep moves the siren schedule forward one step: end of a cycle, pause,
// start of the next cycle, or end of the incident.
func (e *Engine) sirenStep(ref time.Time) []Action {
	inc := e.s.Incident
	if inc == nil { // should not happen: avoid looping
		e.s.Deadline = time.Time{}
		return nil
	}
	if inc.SirenOn {
		out := []Action{SirenOff{}}
		if inc.Cycle >= e.cfg.SirenCycles {
			out = append(out, Emit{Cleared{Incident: inc.ID, By: ClearedByTimeout, Duration: ref.Sub(inc.StartedAt)}})
			if endsWithArming(inc.Kind) {
				e.s.TimedOut = append(e.s.TimedOut, inc.ID)
			}
			out = append(out, e.endIncident(ref)...)
			return append(out, e.wake(ref)...)
		}
		inc.SirenOn = false
		e.s.Deadline = ref.Add(e.cfg.SirenPause)
		return out
	}
	inc.Cycle++
	inc.SirenOn = true
	e.s.Deadline = ref.Add(e.cfg.SirenCycle)
	return []Action{
		SirenOn{Duration: e.cfg.SirenCycle},
		Emit{SirenCycleStarted{Incident: inc.ID, Cycle: inc.Cycle, Of: e.cfg.SirenCycles}},
	}
}

// endsWithArming tells whether an alarm of this kind belongs to the arming it
// happened in, so that the disarm ending that arming ends it too, even after its
// siren ran out. An intrusion and a tamper while armed do: they are what the arming
// watches for, and the person who disarms is back in charge of the place. A fire and
// a panic do not: they are watched day and night, and disarming the shop later says
// nothing of whether someone saw them. Their escalation runs until a contact answers.
func endsWithArming(kind AlertKind) bool {
	return kind == AlertIntrusion || kind == AlertTamper
}

// endIncident closes the incident, when its siren ran out or a code silenced it, and
// goes back to the mode held before it: disarmed (see resumePlan), the exit delay (see
// resumeArming) or armed, with what is left of an entry delay it interrupted.
func (e *Engine) endIncident(at time.Time) []Action {
	var back Mode
	if e.s.Incident != nil {
		back = e.s.Incident.ReturnTo
	}
	e.s.Incident, e.s.Raised, e.s.Sounding = nil, nil, nil
	switch back {
	case ModeDisarmed:
		e.resumePlan(at)
		return nil
	case ModeArming:
		return e.resumeArming(at)
	}
	e.s.Deadline = time.Time{}
	e.setMode(ModeArmed, at)
	if e.s.EntryCause != nil && !e.s.EntryEnds.IsZero() {
		// The alarm interrupted an entry delay that has not ended yet: it goes on.
		e.setMode(ModeEntryDelay, at)
		e.s.Deadline, e.s.EntryEnds = e.s.EntryEnds, time.Time{}
	}
	return nil
}

// resumePlan goes back to disarmed with the automatic arming planned before the alarm:
// nobody chose not to arm when the siren ran out, nor when a code silenced it (a disarm
// does choose, and plans again from its time). An alarm that ends after the closing
// time must not cancel tonight's arming, the very risk the plan removes: an arming
// whose time went by during the alarm starts now, with a whole exit delay, within the
// plan's limit, as one missed while the hub was off (see checkPlan and scheduleStep).
func (e *Engine) resumePlan(at time.Time) {
	sch := e.s.Schedule
	e.disarm(at)
	if sch != nil {
		e.s.Schedule = sch
		e.checkPlan(at)
	}
}

// resumeArming gives the exit delay back once an alarm raised during it ends, whole and
// from now: the people inside spent it on the alarm (a smoke false alarm silenced at the
// keypad). Arming at once would set off an intrusion with their next step, and skip
// what the end of the delay does (see finishArming). An automatic arming keeps its
// plan, so that it can still be postponed.
func (e *Engine) resumeArming(at time.Time) []Action {
	sch := e.s.Schedule
	ev := e.beginArming(at, e.s.ArmOrigin, e.s.ArmUser, nil)
	e.s.Schedule = sch
	return []Action{ev}
}

// lateEntry ends an entry delay that another alarm interrupted, and that no code
// ended: whoever came in is an intruder. The intrusion alerts under its own incident,
// with an intrusion's escalation, as one raised during that alarm (see during).
func (e *Engine) lateEntry(at time.Time) []Action {
	cause := *e.s.EntryCause
	e.s.EntryCause, e.s.EntryEnds = nil, time.Time{}
	return e.during(at, cause, AlertIntrusion)
}

// clearIncident stops the siren on a user's order: the person ended the incident,
// whatever its kind; ended says the order was alarm.end (see end).
func (e *Engine) clearIncident(at time.Time, user string, ended bool) []Action {
	inc := e.s.Incident
	if inc == nil {
		return []Action{SirenOff{}}
	}
	return []Action{
		SirenOff{},
		Emit{Cleared{Incident: inc.ID, By: ClearedByUser, User: user, Duration: at.Sub(inc.StartedAt), Ended: ended}},
		Ended{Incidents: []string{inc.ID}},
	}
}

func (e *Engine) alert(kind AlertKind, cause *Cause) Alert {
	return Alert{Incident: e.newID(), Kind: kind, Urgency: urgency(kind), Cause: cause}
}

func urgency(kind AlertKind) Urgency {
	switch kind {
	case AlertFire, AlertPanic:
		return UrgencyCritical
	}
	return UrgencyHigh
}
