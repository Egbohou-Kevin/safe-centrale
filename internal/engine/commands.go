package engine

import (
	"maps"
	"slices"
	"time"
)

func (e *Engine) command(c Command) []Action {
	if !c.Exp.IsZero() && c.Now.After(c.Exp) {
		return []Action{reply(c, StatusExpired, "")}
	}
	switch c.Name {
	case CmdArm:
		return e.arm(c)
	case CmdArmNow:
		return e.armNow(c)
	case CmdBypass:
		return e.bypass(c)
	case CmdPostpone:
		return e.postpone(c)
	case CmdDisarm, CmdSilence:
		return e.disarmOrSilence(c)
	}
	return []Action{reply(c, StatusFailed, ReasonUnknownCommand)}
}

func reply(c Command, status Status, reason Reason) Reply {
	return Reply{Ref: c.ID, Status: status, Reason: reason}
}

func done(c Command) []Action { return []Action{reply(c, StatusDone, "")} }

func refused(c Command, reason Reason) []Action {
	return []Action{reply(c, StatusRefused, reason)}
}

// --- arming ------------------------------------------------------------------

func (e *Engine) arm(c Command) []Action {
	switch e.s.Mode {
	case ModeArming, ModeArmed:
		return done(c) // already armed: nothing to do
	case ModeEntryDelay, ModeTriggered:
		return refused(c, ReasonWrongState)
	}
	open := e.blockingOpen()
	if len(open) > 0 && !c.Force {
		r := reply(c, StatusRefused, ReasonZoneOpen)
		r.Detail = map[string]any{"devices": open}
		return []Action{r}
	}
	return []Action{e.beginArming(c.Now, c.Origin, c.User, open), reply(c, StatusDone, "")}
}

// beginArming starts the exit delay. The open zones given are bypassed (forced
// arming) and reported in the event.
func (e *Engine) beginArming(at time.Time, origin Origin, user string, open []string) Emit {
	for _, id := range open {
		e.s.Bypassed[id] = true
	}
	e.setMode(ModeArming, at)
	e.s.Deadline = at.Add(e.cfg.ExitDelay)
	e.s.ArmOrigin, e.s.ArmUser = origin, user

	bypassed := slices.Sorted(maps.Keys(e.s.Bypassed))
	if bypassed == nil {
		bypassed = []string{}
	}
	return Emit{ArmingStarted{
		Origin: origin, User: user, EndsAt: e.s.Deadline,
		Forced: len(open) > 0, Bypassed: bypassed,
	}}
}

// blockingOpen returns the open doors and windows that prevent arming.
func (e *Engine) blockingOpen() []string {
	var ids []string
	for id := range e.s.Open {
		kind := e.cfg.Zones[id].Kind
		if (kind == ZoneEntry || kind == ZoneInstant) && !e.s.Bypassed[id] {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

func (e *Engine) armNow(c Command) []Action {
	switch e.s.Mode {
	case ModeArmed:
		return done(c)
	case ModeArming:
		e.s.ArmOrigin, e.s.ArmUser = c.Origin, c.User
		return append(e.finishArming(c.Now), reply(c, StatusDone, ""))
	}
	return refused(c, ReasonWrongState)
}

// finishArming ends the exit delay. Contacts report changes only, so a zone opened
// during the exit delay and still open must be dealt with now or never: an entry door
// starts the entry delay, as if it had just opened; a window is bypassed and the
// contacts are alerted, as the automatic arming does, since nobody is at the keypad
// any more to be told.
func (e *Engine) finishArming(at time.Time) []Action {
	e.setMode(ModeArmed, at)
	e.s.Deadline = time.Time{}
	out := []Action{Emit{Armed{Origin: e.s.ArmOrigin, User: e.s.ArmUser}}}

	var doors, windows []string
	for _, id := range e.blockingOpen() {
		if e.cfg.Zones[id].Kind == ZoneEntry {
			doors = append(doors, id)
		} else {
			windows = append(windows, id)
		}
	}
	if len(windows) > 0 {
		for _, id := range windows {
			e.s.Bypassed[id] = true
		}
		al := e.alert(AlertArmingBypassed, nil)
		al.Devices = windows
		out = append(out, al)
	}
	if len(doors) > 0 {
		cause := Cause{DeviceID: doors[0], Sensor: SensorContact, Zone: e.cfg.Zones[doors[0]].Name}
		out = append(out, e.armedZone(at, ZoneEntry, cause)...)
	}
	return out
}

func (e *Engine) bypass(c Command) []Action {
	if e.s.Mode != ModeDisarmed && e.s.Mode != ModeArming {
		return refused(c, ReasonWrongState)
	}
	for _, id := range c.Devices {
		e.s.Bypassed[id] = true
	}
	return done(c)
}

// --- disarming -----------------------------------------------------------------

func (e *Engine) disarmOrSilence(c Command) []Action {
	user, refusal := e.authorize(c)
	if refusal != nil {
		return refusal
	}
	if c.Name == CmdSilence {
		return e.silence(c, user)
	}
	if e.s.Mode == ModeDisarmed {
		return done(c)
	}
	var out []Action
	var ended []string
	if e.s.Mode == ModeTriggered && e.s.Incident != nil {
		ended = append(ended, e.s.Incident.ID)
		out = e.clearIncident(c.Now, user)
	}
	// The alarms of this arming whose siren already ran out end with it: the person
	// who disarms is back in charge, whatever the siren did in between.
	if len(e.s.TimedOut) > 0 {
		ended = append(ended, e.s.TimedOut...)
		out = append(out, Ended{Incidents: slices.Clone(e.s.TimedOut)})
	}
	e.disarm(c.Now)
	out = append(out, Emit{Disarmed{Origin: c.Origin, User: user, Incidents: ended}}, reply(c, StatusDone, ""))
	return append(out, e.wake(c.Now)...)
}

// silence stops the siren: the incident ends and the alarm goes back to the mode it
// was in, without disarming (see endIncident). A fire that waited behind it rings in
// turn (see wake).
func (e *Engine) silence(c Command, user string) []Action {
	if e.s.Mode != ModeTriggered {
		return refused(c, ReasonWrongState)
	}
	out := e.clearIncident(c.Now, user)
	out = append(out, e.endIncident(c.Now)...)
	out = append(out, reply(c, StatusDone, ""))
	return append(out, e.wake(c.Now)...)
}

// authorize checks the right to disarm or to silence the siren and tells who it
// is. A wrong code is counted (wrongCode); a good one starts both counts over. A
// non-nil refusal holds every action to return instead.
func (e *Engine) authorize(c Command) (user string, refusal []Action) {
	if c.Now.Before(e.s.LockedUntil) {
		return "", refused(c, ReasonLockedOut)
	}
	if c.Auth.Checked && c.Auth.OK {
		e.s.Failures, e.s.WrongCodes = nil, 0
		if c.Auth.User != "" {
			return c.Auth.User, nil
		}
		return c.User, nil
	}
	return "", e.wrongCode(c)
}

// wrongCode counts a wrong code twice, wherever it was entered: the lockout is the
// site's. LockoutAttempts of them within LockoutWindow lock disarming: a mistake from
// this morning does not bring a lockout closer this evening. The count since the last
// good code ignores time: every LockoutAttempts of them alert the contacts, even
// without a lockout, so that a patient guesser who stays under the window (the cloud
// lets one phone try two codes every 10 minutes) is still heard of. One wrong code
// raises one alert at most.
func (e *Engine) wrongCode(c Command) []Action {
	e.s.Failures = slices.DeleteFunc(e.s.Failures, func(at time.Time) bool { return !c.Now.Before(at.Add(LockoutWindow)) })
	e.s.Failures = append(e.s.Failures, c.Now)
	e.s.WrongCodes++
	failure := AuthFailed{Origin: c.Origin, Attempts: len(e.s.Failures)}
	locks := len(e.s.Failures) >= e.cfg.LockoutAttempts
	if locks {
		e.s.LockedUntil = c.Now.Add(e.cfg.LockoutDuration)
		e.s.Failures = nil
		failure.LockedUntil = e.s.LockedUntil
	}
	out := []Action{Emit{failure}}
	if locks || e.s.WrongCodes%e.cfg.LockoutAttempts == 0 {
		out = append(out, e.alert(AlertAuthFailed, nil))
	}
	return append(out, reply(c, StatusRefused, ReasonBadCode))
}
