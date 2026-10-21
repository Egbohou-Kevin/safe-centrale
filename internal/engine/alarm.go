package engine

import "time"

// signal applies a sensor change. A device's tamper sensor always means tamper,
// whatever its zone and even without one; any other sensor counts only if the
// device's zone reacts to it. Contacts are tracked open or closed in every mode, so
// that arming knows which doors are open.
func (e *Engine) signal(sig Signal) []Action {
	zone, known := e.cfg.Zones[sig.DeviceID]
	if sig.Sensor == SensorTamper {
		if !sig.Active {
			delete(e.s.Tampered, sig.DeviceID)
			return nil
		}
		e.s.Tampered[sig.DeviceID] = true
		return e.tamper(sig.Now, Cause{DeviceID: sig.DeviceID, Sensor: sig.Sensor, Zone: zone.Name})
	}
	if !known || !Accepts(zone.Kind, sig.Sensor) {
		return nil
	}
	e.trackContact(sig)
	if !sig.Active {
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

// forget drops a device removed from the site: it no longer holds a door open or a
// bypass. What the incidents remember of it stays.
func (e *Engine) forget(id string) {
	delete(e.s.Open, id)
	delete(e.s.Bypassed, id)
	delete(e.s.Tampered, id)
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
			return []Action{Emit{EntryDelayStarted{DeviceID: cause.DeviceID, EndsAt: e.s.Deadline}}}
		}
		return e.trigger(at, cause, AlertIntrusion)

	case ModeEntryDelay:
		if kind == ZoneInstant { // a window during the entry delay: no grace
			return e.trigger(at, cause, AlertIntrusion)
		}
		// Motion from the person coming in is expected, and a second opening of the
		// entry door does not restart the delay.
	}
	return nil // disarmed, exit delay, or already triggered
}

// trigger sets the alarm off. If it is already ringing there is nothing to redo,
// except for a fire or a panic, which must alert even during another alarm.
func (e *Engine) trigger(at time.Time, cause Cause, kind AlertKind) []Action {
	if e.s.Mode == ModeTriggered {
		if inc := e.s.Incident; inc != nil && kind != inc.Kind && (kind == AlertFire || kind == AlertPanic) {
			return []Action{e.alert(kind, &cause)}
		}
		return nil
	}
	returnTo := ModeArmed
	if e.s.Mode == ModeDisarmed {
		returnTo = ModeDisarmed
	}
	e.setMode(ModeTriggered, at)
	e.s.EntryCause = nil
	inc := &Incident{
		ID: e.newID(), Kind: kind, Cause: cause, StartedAt: at,
		Cycle: 1, SirenOn: true, ReturnTo: returnTo,
	}
	e.s.Incident = inc
	e.s.Deadline = at.Add(e.cfg.SirenCycle)
	last := cause
	e.s.LastTrigger, e.s.LastTriggerAt = &last, at

	return []Action{
		Emit{Triggered{Incident: inc.ID, Cause: cause}},
		Emit{SirenCycleStarted{Incident: inc.ID, Cycle: 1, Of: e.cfg.SirenCycles}},
		SirenOn{Duration: e.cfg.SirenCycle},
		Alert{Incident: inc.ID, Kind: kind, Urgency: urgency(kind), Cause: &cause},
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
			e.endIncident(ref)
			return out
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

// endIncident closes the incident and goes back to the mode held before it.
func (e *Engine) endIncident(at time.Time) {
	if e.s.Incident != nil && e.s.Incident.ReturnTo == ModeDisarmed {
		e.disarm(at)
		return
	}
	e.s.Incident = nil
	e.s.Deadline = time.Time{}
	e.setMode(ModeArmed, at)
}

// clearIncident stops the siren on a user's order.
func (e *Engine) clearIncident(at time.Time, user string) []Action {
	inc := e.s.Incident
	if inc == nil {
		return []Action{SirenOff{}}
	}
	return []Action{
		SirenOff{},
		Emit{Cleared{Incident: inc.ID, By: ClearedByUser, User: user, Duration: at.Sub(inc.StartedAt)}},
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
