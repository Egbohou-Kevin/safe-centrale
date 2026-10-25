package engine

import (
	"slices"
	"time"
)

// Boot resumes operation after a restart, once Restore has been called. current
// is the last known state of the devices (the retained state on the broker), each
// dated when it began (Signal.Now, the zero time when unknown).
//
// The contacts' states are learned first, without acting on them, so that the
// deadlines missed while the hub was off are replayed against the doors as they are
// now: a door closed during the outage does not start the entry delay at the end of
// the exit delay. Then the devices still in alarm trip the engine like an ordinary
// signal, except what was already known before the restart: a tamper already
// reported, a contact already open, a state older than what the engine heard (see
// older). Contacts report changes only, so a door known open and still open is not
// news: an interior door left open at arming, or the entry door left open after the
// siren was silenced, must not set off a restart while armed.
//
// While disarmed, the automatic arming saved before the restart is checked against
// the configuration first (see checkPlan): a new configuration is applied by a
// restart.
//
// Momentary sensors (motion) must therefore publish their return to rest, or a
// stale "active" state would trigger an alarm on restart.
func (e *Engine) Boot(now time.Time, current []Signal) []Action {
	e.awaitingBoot = false
	e.s.Checking = nil // the checks under way did not survive the restart
	e.pullBack(now)
	alarms := e.learn(now, current)
	if e.s.Mode == ModeDisarmed {
		e.checkPlan(now)
	}
	out := e.tick(now)
	if e.sirenSounds() && !slices.ContainsFunc(out, isSirenOn) {
		// The siren may have missed its order while the hub went down: it is told
		// again, for what is left of the cycle.
		out = append(out, SirenOn{Duration: e.s.Deadline.Sub(now)})
	}
	for _, sig := range alarms {
		sig.Now = now
		out = append(out, e.signal(sig)...)
	}
	return out
}

// learn records the devices' states given at Boot: which contacts are open, which
// tampers have ended, which detectors no longer hold the fire that rings. It returns
// the signals still to act on: every active one but the tampers already reported
// before the restart, the contacts already known open and the states older than what
// the engine knows. A state changed between Restore and Boot is this run's own news
// (see early): applied to the saved state, it may have been dropped there, as the
// alarm already ringing when the hub stopped.
func (e *Engine) learn(now time.Time, current []Signal) []Signal {
	var alarms []Signal
	for _, sig := range current {
		early := e.early[sensorKey(sig.DeviceID, sig.Sensor)]
		if !early && e.older(now, sig) {
			continue
		}
		if sig.Sensor == SensorTamper {
			if !sig.Active {
				delete(e.s.Tampered, sig.DeviceID)
				continue
			}
			if e.s.Tampered[sig.DeviceID] {
				continue
			}
		}
		known := sig.Sensor == SensorContact && e.s.Open[sig.DeviceID] && !early
		e.trackContact(sig)
		if !sig.Active {
			e.dropSounding(func(c Cause) bool { return c.DeviceID == sig.DeviceID && c.Sensor == sig.Sensor })
		}
		if sig.Active && !known {
			alarms = append(alarms, sig)
		}
	}
	e.early = nil
	return alarms
}

// older tells whether a state given at Boot is older than what the engine knows. The
// broker saves its retained messages every few minutes: after an unclean stop (a power
// cut, a reboot by the watchdog) it gives back each one as it was at its last save. A
// door closed after that save would come back open, and be taken for one opened while
// the hub was off: a siren the moment the power returns. So would a smoke detector that
// cleared, and a movement whose return to rest came after the save.
//
// A door, a window, a smoke detector or a tamper is older than the last state the
// engine took from it (Heard). A momentary sensor (a movement, a vibration, glass
// breaking) keeps no such time, which every passer-by would rewrite: its state is
// older when it began before the mode the engine is in, or during the exit delay under
// way. Live, it was heard then, and did what it does in that mode; active since, it is
// no new movement. A state that does not say when it began is trusted, as is one dated
// after now, the engine's times then being of no use: a clock that ran ahead.
func (e *Engine) older(now time.Time, sig Signal) bool {
	if sig.Now.IsZero() {
		return false
	}
	if heard, ok := e.s.Heard[sensorKey(sig.DeviceID, sig.Sensor)]; ok {
		return !sig.Now.After(heard)
	}
	if lasting(sig.Sensor) || !sig.Active {
		return false
	}
	from := e.s.Since
	if e.s.Mode == ModeArming {
		from = later(from, e.s.Deadline)
	}
	return !from.After(now) && !sig.Now.After(from)
}

// hear records when the engine took the state of a sensor that keeps one in every
// mode: a door or a window (its Open moves with each anyway), a smoke detector, a
// tamper. Between Restore and Boot, the change is marked as this run's own (early).
func (e *Engine) hear(sig Signal) {
	key := sensorKey(sig.DeviceID, sig.Sensor)
	if e.awaitingBoot {
		if e.early == nil {
			e.early = map[string]bool{}
		}
		e.early[key] = true
	}
	if !lasting(sig.Sensor) || sig.Now.IsZero() {
		return
	}
	if e.s.Heard == nil {
		e.s.Heard = map[string]time.Time{}
	}
	e.s.Heard[key] = sig.Now
}

// lasting tells whether a sensor holds its state until it changes, in every mode: a
// contact, a smoke detector, a tamper. The others (a movement, a vibration, glass
// breaking) are moments.
func lasting(s SensorKind) bool {
	return s == SensorContact || s == SensorSmoke || s == SensorTamper
}

func sensorKey(deviceID string, s SensorKind) string { return deviceID + "/" + string(s) }
