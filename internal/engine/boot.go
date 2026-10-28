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
// booted says when the system started (see Booted): a Pi that keeps no time starts
// from an old one, and a state dated before booted.At was dated before the clock went
// back, on the clock of the engine's own times (see behind), unless NTP set the clock
// forward before the hub started (see bootRef.stepped). Unknown, or after now (a wrong
// reading), the states dated up to now are trusted as before. The trace of that step
// (booted.Shown) serves the first Boot of a system run only (see Snapshot.BootShown): at
// a later one, a live step forward since would widen it, and every state the engine
// heard before that step would look dated before NTP set the clock, news: a movement
// heard rang again (28 October 2026).
func (e *Engine) Boot(now time.Time, booted Booted, current []Signal) []Action {
	e.awaitingBoot = false
	e.s.Checking = nil // the checks under way did not survive the restart
	ref := bootRef{now: now, booted: booted.At, shown: booted.Shown, since: e.s.Since}
	if booted.At.After(now) {
		ref.booted = time.Time{}
	}
	// No step forward, a wrong reading, or the trace a Boot of this system run already
	// had, with or without a step: the rule without it. Kept even when it tells no step:
	// NTP may set the clock forward while the hub runs, and the same trace, given again,
	// would then look before the start.
	if ref.booted.IsZero() || !ref.shown.Before(ref.booted) || booted.Shown.Equal(e.s.BootShown) {
		ref.shown = time.Time{}
	}
	e.s.BootShown = booted.Shown
	e.pullBack(now) // brings Since back if the clock went back behind it: ref keeps it
	alarms := e.learn(ref, current)
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
		began := sig.Now
		sig.Now = now
		out = append(out, e.signal(sig)...)
		if key := sensorKey(sig.DeviceID, sig.Sensor); e.s.Moving[key].Equal(now) {
			e.move(key, Signal{Active: true, Now: began}) // as the broker keeps it
		}
	}
	return out
}

// bootRef is what Boot compares the devices' states with: now, when the system started
// (zero when unknown) and the start of the mode as the run before left it, on its
// clock: brought back to the time its clock went back to if that run found it after
// its now (see pullSinceBack), as Boot found it otherwise.
type bootRef struct {
	now, booted, since time.Time
	// shown is when the system started on the clock as it read early at boot, before
	// NTP set it forward: before booted, the zero time otherwise (see stepped).
	shown time.Time
}

// Booted is when the system started, as Boot needs it.
type Booted struct {
	// At is when the system started on the clock as it is now: now less the system's
	// uptime. The zero time when unknown.
	At time.Time
	// Shown is when the system started on the clock as it read early at boot, before
	// NTP set it: before At when NTP set the clock forward since. Given whatever it
	// says, Boot keeps it (see Snapshot.BootShown) and takes it for a step only when
	// before At. The zero time when unknown.
	Shown time.Time
}

// stepped tells whether a state dated at t may be dated on the clock the system
// started with, before NTP set it forward, while ref, a time of the engine's, was not
// set on the clock as it is since: the two do not compare.
//
// NTP may set the clock forward before the hub starts (the services that date the
// devices' states start before it): booted, taken on the clock set right, is then later
// than shown, the time the clock read at boot, and a door opened in the seconds after
// the power came back is dated before booted, on the clock as it read then. Compared
// with a time heard before the power cut, it would look older and be lost. It was read
// before the step, before now: at most the uptime (now less booted) after shown. Such a
// state is therefore news unless it bears ref itself.
//
// A time of the engine's since booted, up to now, is on the clock set right: the state
// compares with it once brought onto that clock (see mended). Not every such time was
// set after the step: the end of an exit delay was set before the power cut, ahead. One
// before booted (heard before the power cut) or after now (set before the clock went
// back) does not compare. The price: a state the broker kept from before the power cut,
// dated less than the uptime after the time the clock went back to, rings again
// (docs/engine.md, 28 October 2026).
func (r bootRef) stepped(t, ref time.Time) bool {
	return r.beforeStep(t) && (ref.Before(r.booted) || ref.After(r.now))
}

// beforeStep tells whether a state dated at t may be dated on the clock the system
// started with, before NTP set it forward (see stepped).
func (r bootRef) beforeStep(t time.Time) bool {
	return !r.shown.IsZero() && !t.Before(r.shown) && t.Before(r.booted) && t.Before(r.shown.Add(r.now.Sub(r.booted)))
}

// mended brings a state dated at t onto the clock set right when it may be dated on the
// clock as it read at boot (see beforeStep): as long after booted as it was after
// shown. Any other time is given back as is. A movement in the seconds after the end of
// an exit delay that ended while the system started, dated before the step, is later
// than that end: compared as dated, it looked older and was lost.
func (r bootRef) mended(t time.Time) time.Time {
	if !r.beforeStep(t) {
		return t
	}
	return r.booted.Add(t.Sub(r.shown))
}

// learn records the devices' states given at Boot: which contacts are open, which
// tampers have ended, which detectors no longer hold the fire that rings. It returns
// the signals still to act on: every active one but the tampers already reported
// before the restart, the contacts already known open and the states older than what
// the engine knows. A state changed between Restore and Boot is this run's own news
// (see early): applied to the saved state, it may have been dropped there, as the
// alarm already ringing when the hub stopped. The active states set aside are kept
// (SetAside), and so are those the last Boot set aside, given back again.
func (e *Engine) learn(ref bootRef, current []Signal) []Signal {
	var alarms []Signal
	before := e.s.SetAside
	e.s.SetAside = nil
	for _, sig := range current {
		key := sensorKey(sig.DeviceID, sig.Sensor)
		early := e.early[key]
		if !early && e.watches(sig) {
			e.move(key, sig) // the broker's state, the time it began included
		}
		again := sig.Active && !sig.Now.IsZero() && sig.Now.Equal(before[key])
		if !early && (again || e.older(ref, sig)) {
			if sig.Active && !sig.Now.IsZero() {
				if e.s.SetAside == nil {
					e.s.SetAside = map[string]time.Time{}
				}
				e.s.SetAside[key] = sig.Now
			}
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
// pullBack). The system's start, when known, tells a state dated before the clock went
// back from one dated since; not for a time set apart by a run before this one
// (apartBefore), the broker's state may be dated on that run's clock. The start of the
// mode is the one the run before left (bootRef.since), on its own clock. The clock set
// forward by NTP before the hub started, a state dated on the clock as it read at boot
// is news against a time of the engine's on another clock (see bootRef.stepped), and is
// brought onto the clock set right against one since the system started, up to now
// (see bootRef.mended): the end of an exit delay that ended meanwhile.
func (e *Engine) older(ref bootRef, sig Signal) bool {
	now, booted := ref.now, ref.booted
	if sig.Now.IsZero() {
		return false
	}
	key := sensorKey(sig.DeviceID, sig.Sensor)
	if heard, ok := e.s.Heard[key]; ok {
		if ref.stepped(sig.Now, heard) {
			return sig.Now.Equal(heard)
		}
		return !ref.mended(sig.Now).After(heard)
	}
	if ahead, ok := e.s.HeardAhead[key]; ok {
		before := booted
		if e.apartBefore[key] {
			before = time.Time{}
		}
		if ref.stepped(sig.Now, ahead) {
			if sig.Now.Equal(ahead) {
				return true
			}
		} else if behind(sig.Now, ahead, now, before) {
			return true
		}
	}
	if lasting(sig.Sensor) || !sig.Active {
		return false
	}
	from := ref.since
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
	if ref.stepped(sig.Now, from) {
		return sig.Now.Equal(from)
	}
	if from.After(now) {
		return behind(sig.Now, from, now, booted)
	}
	return !ref.mended(sig.Now).After(from)
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
	delete(e.s.SetAside, key) // the broker's state is this one now
	if e.awaitingBoot {
		if e.early == nil {
			e.early = map[string]bool{}
		}
		e.early[key] = true
	}
	if momentary(sig.Sensor) {
		e.move(key, sig)
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

// momentary tells whether a sensor's state is a moment that keeps no time in the engine
// but its active one (Moving): a movement, a vibration, glass breaking. A panic button
// keeps Heard instead.
func momentary(s SensorKind) bool { return !lasting(s) && s != SensorPanic }

// watches tells whether the engine takes the momentary state of sig: a sensor its zone
// reacts to, as signal hears it.
func (e *Engine) watches(sig Signal) bool {
	zone, known := e.cfg.Zones[sig.DeviceID]
	return momentary(sig.Sensor) && known && Accepts(zone.Kind, sig.Sensor)
}

// move records the state of a momentary sensor (Moving): active, by the time it began;
// at rest, or not saying when it began, it is dropped.
func (e *Engine) move(key string, sig Signal) {
	if !sig.Active || sig.Now.IsZero() {
		delete(e.s.Moving, key)
		return
	}
	if e.s.Moving == nil {
		e.s.Moving = map[string]time.Time{}
	}
	e.s.Moving[key] = sig.Now
}

func sensorKey(deviceID string, s SensorKind) string { return deviceID + "/" + string(s) }
