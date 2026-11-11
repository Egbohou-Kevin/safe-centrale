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
	e.s.Schedule = &Schedule{Closing: at, At: at, Limit: at.Add(e.cfg.AutoArmLimit)}
	e.scheduleDeadline(from)
}

// missedBy is how late a deadline of the automatic arming must come for it to count
// as missed: the hub was off (or stuck) at its time, not merely a little late, as the
// driver's ticks always are.
const missedBy = time.Minute

// checkPlan checks the automatic arming saved before a restart against the
// configuration, which may have changed: a configuration from the cloud is applied by
// a restart. A plan for a closing time the configuration no longer has (moved, or no
// closing that day any more) is made again from now, the first closing after the
// restart; a plan whose closing still stands keeps its postponements, under the
// configuration's limit and notice. A missing plan is made.
func (e *Engine) checkPlan(now time.Time) {
	sch := e.s.Schedule
	if sch == nil {
		e.planSchedule(now)
		return
	}
	closing := sch.Closing
	if closing.IsZero() { // saved before the plan named its closing
		closing = sch.Limit.Add(-e.cfg.AutoArmLimit)
	}
	from := now
	if !now.Before(closing) {
		from = closing.Add(-time.Nanosecond)
	}
	if at, ok := e.cfg.nextClosing(from); !ok || !at.Equal(closing) || now.After(closing.Add(e.cfg.AutoArmLimit)) {
		e.planSchedule(now) // a closing changed, or missed past its limit (see scheduleStep)
		return
	}
	sch.Closing, sch.Limit = closing, closing.Add(e.cfg.AutoArmLimit)
	if sch.At.After(sch.Limit) {
		sch.At = sch.Limit
	}
	e.scheduleDeadline(now)
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
// the arming. Both are replayed at their own time when a little late. One missed
// because the hub was off (now is the time of the tick) is not: an arming missed past
// the plan's limit is dropped and the next closing planned, so that a hub back at 8 in
// the morning does not arm a shop that opened again; one missed within the limit
// starts its exit delay now, so that the people inside still get the whole delay to
// postpone it or to leave.
func (e *Engine) scheduleStep(ref, now time.Time) []Action {
	sch := e.s.Schedule
	if sch == nil { // should not happen: avoid looping
		e.s.Deadline = time.Time{}
		return nil
	}
	missed := now.Sub(ref) > missedBy
	if missed && now.After(sch.Limit) {
		e.planSchedule(now)
		return nil
	}
	if !sch.Announced {
		sch.Announced = true
		e.s.Deadline = sch.At
		return []Action{Emit{ArmingScheduled{ScheduledFor: sch.At}}}
	}
	if missed {
		sch.At = now // what a postponement starts from
		return e.autoArm(now)
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
