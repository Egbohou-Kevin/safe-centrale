package engine

import "time"

// planSchedule plans the first closing time after from. It also sets the deadline
// of the disarmed mode, which is the advance notice and then the arming itself.
func (e *Engine) planSchedule(from time.Time) {
	e.s.Schedule = nil
	e.s.Deadline = time.Time{}
	at, ok := e.cfg.nextClosing(from)
	if !ok {
		return
	}
	e.s.Schedule = &Schedule{At: at, Limit: at.Add(e.cfg.AutoArmLimit)}
	e.scheduleDeadline(from)
}

// scheduleDeadline points the deadline at the advance notice, or at the arming once
// the notice has been sent. A notice that is already late is sent right away.
func (e *Engine) scheduleDeadline(from time.Time) {
	sch := e.s.Schedule
	if sch.Announced {
		e.s.Deadline = sch.At
		return
	}
	notice := sch.At.Add(-e.cfg.AutoArmNotice)
	if notice.Before(from) {
		notice = from
	}
	e.s.Deadline = notice
}

// scheduleStep handles a deadline while disarmed: first the advance notice, then
// the arming.
func (e *Engine) scheduleStep(ref time.Time) []Action {
	sch := e.s.Schedule
	if sch == nil { // should not happen: avoid looping
		e.s.Deadline = time.Time{}
		return nil
	}
	if !sch.Announced {
		sch.Announced = true
		e.s.Deadline = sch.At
		return []Action{Emit{ArmingScheduled{ScheduledFor: sch.At}}}
	}
	return e.autoArm(ref)
}

// autoArm starts the exit delay at the planned time. It cannot be refused: an
// open zone is bypassed and reported, because forgetting to arm is the very risk
// this feature removes.
func (e *Engine) autoArm(at time.Time) []Action {
	sch := e.s.Schedule
	open := e.blockingOpen()
	out := []Action{e.beginArming(at, OriginSchedule, "", open)}
	e.s.Schedule = sch // kept: the exit delay can still be postponed
	if len(open) > 0 {
		al := e.alert(AlertArmingBypassed, nil)
		al.Devices = open
		out = append(out, al)
	}
	return out
}

// postpone pushes the automatic arming back by one step, within the limits. During
// the exit delay it started, it also cancels that delay: somebody is still inside.
func (e *Engine) postpone(c Command) []Action {
	sch := e.s.Schedule
	if sch == nil || (e.s.Mode != ModeDisarmed && e.s.Mode != ModeArming) {
		return refused(c, ReasonWrongState)
	}
	at := sch.At.Add(e.cfg.PostponeStep)
	if at.After(sch.Limit) {
		at = sch.Limit
	}
	if sch.Postponed >= e.cfg.PostponeMax || !at.After(sch.At) || !at.After(c.Now) {
		return refused(c, ReasonPostponeLimit)
	}
	next := *sch
	next.At, next.Postponed, next.Announced = at, sch.Postponed+1, false

	var out []Action
	if e.s.Mode == ModeArming {
		e.disarm(c.Now)
		out = append(out, Emit{Disarmed{Origin: c.Origin, User: c.User}})
	}
	e.s.Schedule = &next
	e.scheduleDeadline(c.Now)
	return append(out,
		Emit{Postponed{ScheduledFor: at, Count: next.Postponed, Max: e.cfg.PostponeMax}},
		reply(c, StatusDone, ""))
}
