// Package engine is the alarm engine (see docs/engine.md). It is the domain core
// of the hub.
//
// It is deterministic: it does not read the clock, write anywhere or talk to any
// hardware. It is given inputs (signals, orders, ticks) that carry their own time,
// and returns actions for a driver to carry out. Every scenario can therefore be
// tested with a simulated clock, and a real sequence of events can be replayed.
//
// The package knows nothing about MQTT, JSON or the event contract: the wire
// subpackage translates between this domain and the contract. An architecture test
// (internal/archtest) enforces that boundary.
//
// An Engine is not safe for concurrent use: the driver must feed it one input at a
// time.
package engine

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

// Cause is what set off an alarm.
type Cause struct {
	DeviceID string     `json:"device_id"`
	Sensor   SensorKind `json:"sensor"`
	Zone     string     `json:"zone"`
}

// Incident is an alarm in progress, from the first siren to its end.
type Incident struct {
	ID        string    `json:"id"`
	Kind      AlertKind `json:"kind"`
	Cause     Cause     `json:"cause"`
	StartedAt time.Time `json:"started_at"`
	Cycle     int       `json:"cycle"`     // current siren cycle, starting at 1
	SirenOn   bool      `json:"siren_on"`  // false during the pause between two cycles
	ReturnTo  Mode      `json:"return_to"` // mode to go back to when the incident ends
}

// Schedule is the automatic arming planned for the next closing time. It exists
// only while the system is disarmed, and during the exit delay that it started (so
// that this delay can still be postponed).
type Schedule struct {
	At        time.Time `json:"at"`        // when the system arms by itself
	Limit     time.Time `json:"limit"`     // latest time At can be pushed back to
	Postponed int       `json:"postponed"` // postponements used for this closing
	Announced bool      `json:"announced"` // the advance notice has been sent
}

// Snapshot is the full state of the engine. The driver writes it to disk after
// every transition and hands it back to the engine on restart (Restore).
type Snapshot struct {
	Mode     Mode      `json:"mode"`
	Since    time.Time `json:"since"`
	Deadline time.Time `json:"deadline"` // next deadline, zero if none

	ArmOrigin Origin `json:"arm_origin,omitempty"`
	ArmUser   string `json:"arm_user,omitempty"`

	Bypassed   map[string]bool `json:"bypassed"`    // devices bypassed until disarming
	Open       map[string]bool `json:"open"`        // contacts that are currently open
	EntryCause *Cause          `json:"entry_cause"` // what started the entry delay
	Incident   *Incident       `json:"incident"`

	// Memory of the last alarm, kept after the system goes back to armed.
	LastTrigger   *Cause    `json:"last_trigger,omitempty"`
	LastTriggerAt time.Time `json:"last_trigger_at,omitzero"`

	Schedule *Schedule `json:"schedule,omitempty"`

	Failed      int       `json:"failed"`       // consecutive wrong codes
	LockedUntil time.Time `json:"locked_until"` // disarming is refused until this time
}

func (s Snapshot) clone() Snapshot {
	s.Bypassed = maps.Clone(s.Bypassed)
	s.Open = maps.Clone(s.Open)
	if s.EntryCause != nil {
		c := *s.EntryCause
		s.EntryCause = &c
	}
	if s.Incident != nil {
		i := *s.Incident
		s.Incident = &i
	}
	if s.LastTrigger != nil {
		c := *s.LastTrigger
		s.LastTrigger = &c
	}
	if s.Schedule != nil {
		sc := *s.Schedule
		s.Schedule = &sc
	}
	return s
}

func newSnapshot(now time.Time) Snapshot {
	return Snapshot{Mode: ModeDisarmed, Since: now, Bypassed: map[string]bool{}, Open: map[string]bool{}}
}

// State is the public summary of the engine, meant to be published.
type State struct {
	Mode        Mode
	Since       time.Time
	EndsAt      time.Time // end of the exit or entry delay, zero otherwise
	Cycle       int       // current siren cycle while triggered
	Of          int       // number of siren cycles
	LockedUntil time.Time // zero unless disarming is locked

	NextScheduled time.Time // next automatic arming while disarmed, zero if none
}

// Engine is the alarm engine.
type Engine struct {
	cfg   Config
	newID func() string
	s     Snapshot
}

// New creates a disarmed engine. newID produces incident identifiers (for example
// contract.IDGenerator.New).
func New(cfg Config, newID func() string, now time.Time) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if newID == nil {
		return nil, fmt.Errorf("engine: newID is required")
	}
	e := &Engine{cfg: cfg, newID: newID, s: newSnapshot(now)}
	e.planSchedule(now)
	return e, nil
}

// Snapshot returns a copy of the current state.
func (e *Engine) Snapshot() Snapshot { return e.s.clone() }

// Restore replaces the state with a saved one. Call it before Boot.
func (e *Engine) Restore(s Snapshot) {
	s = s.clone()
	if s.Bypassed == nil {
		s.Bypassed = map[string]bool{}
	}
	if s.Open == nil {
		s.Open = map[string]bool{}
	}
	e.s = s
}

// Mode returns the current mode of the alarm.
func (e *Engine) Mode() Mode { return e.s.Mode }

// NextWake returns the time at which the driver must send a Tick, or the zero time
// if nothing is pending.
func (e *Engine) NextWake() time.Time { return e.s.Deadline }

// State returns the public summary of the engine at time now.
func (e *Engine) State(now time.Time) State {
	st := State{Mode: e.s.Mode, Since: e.s.Since}
	switch e.s.Mode {
	case ModeArming, ModeEntryDelay:
		st.EndsAt = e.s.Deadline
	case ModeTriggered:
		if inc := e.s.Incident; inc != nil {
			st.Cycle, st.Of = inc.Cycle, e.cfg.SirenCycles
		}
	}
	if now.Before(e.s.LockedUntil) {
		st.LockedUntil = e.s.LockedUntil
	}
	if e.s.Mode == ModeDisarmed && e.s.Schedule != nil {
		st.NextScheduled = e.s.Schedule.At
	}
	return st
}

// Handle processes an input and returns the actions to carry out. Deadlines that
// have passed are processed first, in order, then the input itself.
func (e *Engine) Handle(in Input) []Action {
	out := e.tick(in.at())
	switch v := in.(type) {
	case Signal:
		out = append(out, e.signal(v)...)
	case Command:
		out = append(out, e.command(v)...)
	}
	return out
}

// Boot resumes operation after a restart, once Restore has been called. current
// is the last known state of the devices (the retained state on the broker).
// Devices already in alarm trip the engine like an ordinary signal.
//
// Momentary sensors (motion) must therefore publish their return to rest, or a
// stale "active" state would trigger an alarm on restart.
func (e *Engine) Boot(now time.Time, current []Signal) []Action {
	out := e.tick(now)
	if e.s.Mode == ModeDisarmed && e.s.Schedule == nil {
		e.planSchedule(now) // a snapshot from before the schedule existed, or a new configuration
	}
	if e.s.Mode == ModeTriggered && e.s.Incident != nil && e.s.Incident.SirenOn {
		out = append(out, SirenOn{Duration: e.s.Deadline.Sub(now)})
	}
	for _, sig := range current {
		sig.Now = now
		out = append(out, e.signal(sig)...)
	}
	return out
}

// --- deadlines ---------------------------------------------------------

func (e *Engine) tick(now time.Time) []Action {
	var out []Action
	for !e.s.Deadline.IsZero() && !now.Before(e.s.Deadline) {
		ref := e.s.Deadline // replay each deadline at its own time, even if the tick is late
		switch e.s.Mode {
		case ModeArming:
			out = append(out, e.finishArming(ref)...)
		case ModeEntryDelay:
			cause := Cause{}
			if e.s.EntryCause != nil {
				cause = *e.s.EntryCause
			}
			out = append(out, e.trigger(ref, cause, AlertIntrusion)...)
		case ModeTriggered:
			out = append(out, e.sirenStep(ref)...)
		case ModeDisarmed:
			out = append(out, e.scheduleStep(ref)...)
		default:
			e.s.Deadline = time.Time{}
		}
	}
	return out
}

// setMode changes the mode. Entering disarmed plans the next automatic arming;
// leaving it drops the plan.
func (e *Engine) setMode(m Mode, at time.Time) {
	e.s.Mode = m
	e.s.Since = at
	if m == ModeDisarmed {
		e.planSchedule(at)
	} else {
		e.s.Schedule = nil
	}
}

// --- automatic arming ---------------------------------------------------

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

func (e *Engine) postpone(c Command) []Action {
	sch := e.s.Schedule
	if sch == nil || (e.s.Mode != ModeDisarmed && e.s.Mode != ModeArming) {
		return []Action{reply(c, StatusRefused, ReasonWrongState, nil)}
	}
	at := sch.At.Add(e.cfg.PostponeStep)
	if at.After(sch.Limit) {
		at = sch.Limit
	}
	if sch.Postponed >= e.cfg.PostponeMax || !at.After(sch.At) || !at.After(c.Now) {
		return []Action{reply(c, StatusRefused, ReasonPostponeLimit, nil)}
	}
	next := *sch
	next.At, next.Postponed, next.Announced = at, sch.Postponed+1, false

	var out []Action
	if e.s.Mode == ModeArming { // somebody is still inside: cancel the exit delay
		e.s.Bypassed = map[string]bool{}
		e.setMode(ModeDisarmed, c.Now)
		out = append(out, Emit{Disarmed{Origin: c.Origin, User: c.User}})
	}
	e.s.Schedule = &next
	e.scheduleDeadline(c.Now)
	return append(out,
		Emit{Postponed{ScheduledFor: at, Count: next.Postponed, Max: e.cfg.PostponeMax}},
		reply(c, StatusDone, "", nil))
}

func (e *Engine) finishArming(at time.Time) []Action {
	e.setMode(ModeArmed, at)
	e.s.Deadline = time.Time{}
	return []Action{Emit{Armed{Origin: e.s.ArmOrigin, User: e.s.ArmUser}}}
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
	returnTo := ModeArmed
	if e.s.Incident != nil && e.s.Incident.ReturnTo == ModeDisarmed {
		returnTo = ModeDisarmed
	}
	e.s.Incident = nil
	e.s.Deadline = time.Time{}
	e.setMode(returnTo, at)
	if returnTo == ModeDisarmed {
		e.s.Bypassed = map[string]bool{}
	}
}

// --- sensor signals -----------------------------------------------------

func (e *Engine) signal(sig Signal) []Action {
	zone, known := e.cfg.Zones[sig.DeviceID]
	if !known {
		return nil
	}
	if sig.Sensor == SensorContact {
		if sig.Active {
			e.s.Open[sig.DeviceID] = true
		} else {
			delete(e.s.Open, sig.DeviceID)
		}
	}
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

	case ZoneTamper:
		if e.s.Mode == ModeDisarmed {
			return []Action{e.alert(AlertTamper, &cause)} // no siren
		}
		return e.trigger(sig.Now, cause, AlertTamper)

	case ZoneEntry, ZoneInstant, ZoneInterior:
		if e.s.Bypassed[sig.DeviceID] {
			return nil
		}
		return e.armedZone(sig.Now, zone.Kind, cause)
	}
	return nil
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

// --- orders ---------------------------------------------------------------

func (e *Engine) command(c Command) []Action {
	if !c.Exp.IsZero() && c.Now.After(c.Exp) {
		return []Action{reply(c, StatusExpired, "", nil)}
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
	return []Action{reply(c, StatusFailed, ReasonUnknownCommand, nil)}
}

func reply(c Command, status Status, reason Reason, detail map[string]any) Reply {
	return Reply{Ref: c.ID, Status: status, Reason: reason, Detail: detail}
}

func (e *Engine) arm(c Command) []Action {
	switch e.s.Mode {
	case ModeArming, ModeArmed:
		return []Action{reply(c, StatusDone, "", nil)} // already armed: nothing to do
	case ModeEntryDelay, ModeTriggered:
		return []Action{reply(c, StatusRefused, ReasonWrongState, nil)}
	}

	open := e.blockingOpen()
	if len(open) > 0 && !c.Force {
		return []Action{reply(c, StatusRefused, ReasonZoneOpen, map[string]any{"devices": open})}
	}
	return []Action{e.beginArming(c.Now, c.Origin, c.User, open), reply(c, StatusDone, "", nil)}
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
		return []Action{reply(c, StatusDone, "", nil)}
	case ModeArming:
		e.s.ArmOrigin, e.s.ArmUser = c.Origin, c.User
		out := e.finishArming(c.Now)
		return append(out, reply(c, StatusDone, "", nil))
	}
	return []Action{reply(c, StatusRefused, ReasonWrongState, nil)}
}

func (e *Engine) bypass(c Command) []Action {
	if e.s.Mode != ModeDisarmed && e.s.Mode != ModeArming {
		return []Action{reply(c, StatusRefused, ReasonWrongState, nil)}
	}
	for _, id := range c.Devices {
		e.s.Bypassed[id] = true
	}
	return []Action{reply(c, StatusDone, "", nil)}
}

// authorize checks the right to disarm or to silence the siren. It counts wrong
// codes and locks disarming after too many mistakes. If ok is false, refusal holds
// every action to return.
func (e *Engine) authorize(c Command) (user string, ok bool, refusal []Action) {
	if c.Now.Before(e.s.LockedUntil) {
		return "", false, []Action{reply(c, StatusRefused, ReasonLockedOut, nil)}
	}
	if c.Auth.Checked && c.Auth.OK {
		e.s.Failed = 0
		if c.Auth.User != "" {
			return c.Auth.User, true, nil
		}
		return c.User, true, nil
	}

	e.s.Failed++
	failure := AuthFailed{Origin: c.Origin, Attempts: e.s.Failed}
	out := []Action{}
	if e.s.Failed >= e.cfg.LockoutAttempts {
		e.s.LockedUntil = c.Now.Add(e.cfg.LockoutDuration)
		e.s.Failed = 0
		failure.LockedUntil = e.s.LockedUntil
		out = append(out, Emit{failure}, e.alert(AlertAuthFailed, nil))
	} else {
		out = append(out, Emit{failure})
	}
	return "", false, append(out, reply(c, StatusRefused, ReasonBadCode, nil))
}

func (e *Engine) disarmOrSilence(c Command) []Action {
	user, ok, refusal := e.authorize(c)
	if !ok {
		return refusal
	}

	if c.Name == CmdSilence {
		if e.s.Mode != ModeTriggered {
			return []Action{reply(c, StatusRefused, ReasonWrongState, nil)}
		}
		out := e.clearIncident(c.Now, user)
		e.endIncident(c.Now) // the siren is off, the alarm stays armed
		return append(out, reply(c, StatusDone, "", nil))
	}

	if e.s.Mode == ModeDisarmed {
		return []Action{reply(c, StatusDone, "", nil)}
	}
	var out []Action
	if e.s.Mode == ModeTriggered {
		out = e.clearIncident(c.Now, user)
	}
	e.s.Incident, e.s.EntryCause = nil, nil
	e.s.Deadline = time.Time{}
	e.s.Bypassed = map[string]bool{}
	e.setMode(ModeDisarmed, c.Now)
	out = append(out, Emit{Disarmed{Origin: c.Origin, User: user}})
	return append(out, reply(c, StatusDone, "", nil))
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
