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
//
// booted is when the system started, on the clock as it is now (now less the
// system's uptime), the zero time when unknown: a Pi that keeps no time starts from an
// old one, and a state dated before booted was dated before the clock went back, on
// the clock of the engine's own times (see behind). Unknown, or after now (a wrong
// reading), the states dated up to now are trusted as before.
func (e *Engine) Boot(now, booted time.Time, current []Signal) []Action {
	e.awaitingBoot = false
	e.s.Checking = nil // the checks under way did not survive the restart
	e.pullBack(now)
	if booted.After(now) {
		booted = time.Time{}
	}
	alarms := e.learn(now, booted, current)
	e.apartBefore = nil
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
func (e *Engine) learn(now, booted time.Time, current []Signal) []Signal {
	var alarms []Signal
	for _, sig := range current {
		early := e.early[sensorKey(sig.DeviceID, sig.Sensor)]
		if !early && e.older(now, booted, sig) {
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
// A door, a window, a smoke detector, a tamper or a panic button is older than the
// last state the engine took from it (Heard). A momentary sensor (a movement, a
// vibration, glass breaking) keeps no such time, which every passer-by would rewrite: its state is
// older when it began before the mode the engine is in, or during the exit delay under
// way (not a panic button, which that delay does not concern). Live, it was heard then, and did what it does in that mode; active since, it is
// no new movement. A state that does not say when it began is trusted.
//
// A time of the engine's after now was set before the clock went back behind it: a Pi
// that keeps no time restarts from an old one, days behind the saved state, until NTP
// sets it right (see behind). The times heard then are kept apart (HeardAhead, see
// pullBack). booted, when the system started (zero when unknown), tells a state dated
// before the clock went back from one dated since; not for a time set apart by a run
// before this one (apartBefore), the broker's state may be dated on that run's clock.
func (e *Engine) older(now, booted time.Time, sig Signal) bool {
	if sig.Now.IsZero() {
		return false
	}
	key := sensorKey(sig.DeviceID, sig.Sensor)
	if heard, ok := e.s.Heard[key]; ok {
		return !sig.Now.After(heard)
	}
	if ahead, ok := e.s.HeardAhead[key]; ok {
		before := booted
		if e.apartBefore[key] {
			before = time.Time{}
		}
		if behind(sig.Now, ahead, now, before) {
			return true
		}
	}
	if lasting(sig.Sensor) || !sig.Active {
		return false
	}
	from := e.s.Since
	if e.s.Mode == ModeArming && sig.Sensor != SensorPanic {
		// The exit delay under way, as far as now while it still runs: its end after
		// now is no clock ahead, and must not make every state trusted. A panic button,
		// a 24-hour zone, owes nothing to the exit delay: pressed during it, it is news.
		end := e.s.Deadline
		if end.After(now) {
			end = now
		}
		from = later(from, end)
	}
	if from.After(now) {
		return behind(sig.Now, from, now, booted)
	}
	return !sig.Now.After(from)
}

// behind tells whether a state that began at began is no later than ref, a time the
// engine set before the clock went back behind it. A state dated after now, or before
// booted (the system's start, zero when unknown), was dated before the clock went back,
// on the same clock as ref: the two compare. One dated since the system started, up to
// now, is news: it is trusted, unless it bears ref itself, the very state the engine
// heard. A real intrusion while the hub was starting must ring. The boot time unknown,
// every state dated up to now is trusted: a door closed after the broker's last save,
// open longer than the clock went back, comes back open and rings (see docs/engine.md,
// 28 October 2026).
func behind(began, ref, now, booted time.Time) bool {
	return !began.After(ref) && (began.After(now) || began.Before(booted) || began.Equal(ref))
}

// hear records when the engine took the state of a sensor that keeps one in every
// mode: a door or a window (its Open moves with each anyway), a smoke detector, a
// tamper; and of a panic button, which a silent panic leaves in the mode it found, so
// that a restart in that mode does not alert it again. Between Restore and Boot, the
// change is marked as this run's own (early).
func (e *Engine) hear(sig Signal) {
	key := sensorKey(sig.DeviceID, sig.Sensor)
	if e.awaitingBoot {
		if e.early == nil {
			e.early = map[string]bool{}
		}
		e.early[key] = true
	}
	if !lasting(sig.Sensor) && sig.Sensor != SensorPanic || sig.Now.IsZero() {
		return
	}
	if e.s.Heard == nil {
		e.s.Heard = map[string]time.Time{}
	}
	e.s.Heard[key] = sig.Now
	delete(e.s.HeardAhead, key)
}

// lasting tells whether a sensor holds its state until it changes, in every mode: a
// contact, a smoke detector, a tamper. The others (a movement, a vibration, glass
// breaking) are moments.
func lasting(s SensorKind) bool {
	return s == SensorContact || s == SensorSmoke || s == SensorTamper
}

func sensorKey(deviceID string, s SensorKind) string { return deviceID + "/" + string(s) }
