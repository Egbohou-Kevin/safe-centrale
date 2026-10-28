package engine

import "time"

// pullBack brings back to now what a clock running ahead stamped in the future: a
// wrong code counts as entered now, a lockout ends at most LockoutDuration from now,
// and a delay or a siren step ends at most its own length from now. Otherwise, once
// the clock is pulled back (an RTC glitch, a bad time at boot corrected by NTP), they
// would hold until it caught up with them: a lockout an hour too long, two forgotten
// mistakes and one real slip locking the site, an exit delay an hour long during
// which a real intrusion does not ring, or a siren told to sound for an hour. The
// time may go back within a run (the hub's loop hands the engine the clock as it is,
// see driver.Options.FollowsClock) or across a restart. The automatic arming is
// planned again instead (checkPlan), at Boot or as soon as the time goes back: a plan
// made on a clock that ran ahead may have skipped today's closing. The times the
// sensors were heard are set apart (HeardAhead): compared as they are, a press stamped
// since on the clock set back would look older than them, and dropped, the very press
// already heard would look new at Boot, and alert again (see older). They are Heard
// again once now reaches them, the clock having caught up with the one they were taken
// on, by a step of NTP or simply running on: a state dated before them then compares
// with them, as a door that came back open from the broker's last save. So is the start
// of the mode (Since, SinceAhead), but not before Boot: Boot compares the states the
// broker kept with it as the run before left it.
func (e *Engine) pullBack(now time.Time) {
	if now.Before(e.seen) && e.s.Mode == ModeDisarmed && !e.awaitingBoot {
		e.checkPlan(now)
	}
	if !e.awaitingBoot {
		e.pullSinceBack(now)
	}
	e.seen = later(e.seen, now)
	for i, at := range e.s.Failures {
		if at.After(now) {
			e.s.Failures[i] = now
		}
	}
	for ref, at := range e.s.Checking {
		if at.After(now) {
			e.s.Checking[ref] = now
		}
	}
	for key, at := range e.s.HeardAhead {
		if !at.After(now) {
			if e.s.Heard == nil {
				e.s.Heard = map[string]time.Time{}
			}
			e.s.Heard[key] = at
			delete(e.s.HeardAhead, key)
		}
	}
	for key, at := range e.s.Heard {
		if at.After(now) {
			if e.s.HeardAhead == nil {
				e.s.HeardAhead = map[string]time.Time{}
			}
			e.s.HeardAhead[key] = at
			delete(e.s.Heard, key)
		}
	}
	if latest := now.Add(e.cfg.LockoutDuration); e.s.LockedUntil.After(latest) {
		e.s.LockedUntil = latest
	}
	if step := e.step(); step > 0 && e.s.Deadline.After(now.Add(step)) {
		e.s.Deadline = now.Add(step)
	}
	if latest := now.Add(e.cfg.EntryDelay); e.s.EntryEnds.After(latest) {
		e.s.EntryEnds = latest
	}
}

// pullSinceBack brings the start of the mode back to now when the clock went back
// behind it, keeping it apart (SinceAhead), and puts it back once the clock catches up
// with it. Left after now, a further restart with the clock set back again would take
// a real movement dated on the clock as it is, after the time it went back to the first
// time, for one older than the mode, and drop it. Brought back while the engine runs,
// the start no longer tells the movements that began before it: they are set aside
// first (see setMomentaryAside).
func (e *Engine) pullSinceBack(now time.Time) {
	if a := e.s.SinceAhead; !a.IsZero() && !a.After(now) {
		e.s.Since, e.s.SinceAhead = a, time.Time{}
	}
	if e.s.Since.After(now) {
		if now.Before(e.seen) {
			e.setMomentaryAside()
		}
		if e.s.SinceAhead.IsZero() {
			e.s.SinceAhead = e.s.Since
		}
		e.s.Since = now
	}
}

// setMomentaryAside sets aside (SetAside) the active momentary states the engine heard
// that a Boot would take for no news (see older): begun before the mode, or during the
// exit delay under way, up to the latest time the engine was handed. Called as the clock
// goes back behind the start of the mode while the engine runs: brought back to the
// time the clock went back to, that start would make them news at the next restart, an
// intrusion alerted for a movement under way before the site armed (the limit written
// in docs/engine.md until 28 October 2026). Given back by the broker with the time
// they began, they are the same states (see setAside: up to liveLag earlier for one
// heard live, which the hub's loop dated after the adapter); a real movement bears
// another time, and a rising edge, its rest heard, drops the entry (see hear). Not at
// Boot: there the broker's states are compared with the start as the run before left it.
func (e *Engine) setMomentaryAside() {
	from := e.s.Since
	if e.s.Mode == ModeArming {
		end := e.s.Deadline
		if end.After(e.seen) {
			end = e.seen
		}
		from = later(from, end)
	}
	for key, began := range e.s.Momentary {
		if began.After(from) {
			continue
		}
		if e.s.SetAside == nil {
			e.s.SetAside = map[string]time.Time{}
		}
		e.s.SetAside[key] = began
	}
}
