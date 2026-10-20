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

func (e *Engine) finishArming(at time.Time) []Action {
	e.setMode(ModeArmed, at)
	e.s.Deadline = time.Time{}
	return []Action{Emit{Armed{Origin: e.s.ArmOrigin, User: e.s.ArmUser}}}
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
	if e.s.Mode == ModeTriggered {
		out = e.clearIncident(c.Now, user)
	}
	e.disarm(c.Now)
	return append(out, Emit{Disarmed{Origin: c.Origin, User: user}}, reply(c, StatusDone, ""))
}

// silence stops the siren: the incident ends and the alarm stays armed.
func (e *Engine) silence(c Command, user string) []Action {
	if e.s.Mode != ModeTriggered {
		return refused(c, ReasonWrongState)
	}
	out := e.clearIncident(c.Now, user)
	e.endIncident(c.Now)
	return append(out, reply(c, StatusDone, ""))
}

// authorize checks the right to disarm or to silence the siren and tells who it
// is. It counts wrong codes and locks disarming after too many mistakes. A non-nil
// refusal holds every action to return instead.
func (e *Engine) authorize(c Command) (user string, refusal []Action) {
	if c.Now.Before(e.s.LockedUntil) {
		return "", refused(c, ReasonLockedOut)
	}
	if c.Auth.Checked && c.Auth.OK {
		e.s.Failed = 0
		if c.Auth.User != "" {
			return c.Auth.User, nil
		}
		return c.User, nil
	}

	e.s.Failed++
	failure := AuthFailed{Origin: c.Origin, Attempts: e.s.Failed}
	if e.s.Failed < e.cfg.LockoutAttempts {
		return "", []Action{Emit{failure}, reply(c, StatusRefused, ReasonBadCode)}
	}
	e.s.LockedUntil = c.Now.Add(e.cfg.LockoutDuration)
	e.s.Failed = 0
	failure.LockedUntil = e.s.LockedUntil
	return "", []Action{Emit{failure}, e.alert(AlertAuthFailed, nil), reply(c, StatusRefused, ReasonBadCode)}
}
