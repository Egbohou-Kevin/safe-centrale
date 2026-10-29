package engine

import "time"

// Event is a fact that happened in the alarm domain. The engine returns events as
// actions; the wire subpackage turns them into contract messages.
type Event interface{ isEvent() }

// ArmingStarted: the exit delay has begun.
type ArmingStarted struct {
	Origin   Origin
	User     string
	EndsAt   time.Time
	Forced   bool     // open zones were bypassed to allow arming
	Bypassed []string // every bypassed device, sorted
}

// Armed: the system is armed.
type Armed struct {
	Origin Origin
	User   string
}

// Disarmed: the system is disarmed. Incidents are those the disarm ended, as Ended
// says them to the escalation: the alarm ringing, and the intrusions and tampers of
// the arming whose siren already ran out. Those who carry the alarm's messages (the
// GSM gateway) learn from it that they have no reason to go.
type Disarmed struct {
	Origin    Origin
	User      string
	Incidents []string
}

// EntryDelayStarted: an entry door opened while armed, the disarm delay is running.
type EntryDelayStarted struct {
	DeviceID string
	Zone     string // the name of the device's zone, as in Cause
	EndsAt   time.Time
}

// Triggered: the alarm went off.
type Triggered struct {
	Incident string
	Cause    Cause
}

// SirenCycleStarted: a siren cycle has begun.
type SirenCycleStarted struct {
	Incident string
	Cycle    int
	Of       int
}

// Cleared: the alarm ended, by a user or because the siren cycles ran out.
type Cleared struct {
	Incident string
	By       ClearedBy
	User     string
	Duration time.Duration
	// Ended: a person ended the alarm with the order alarm.end, without disarming,
	// whether its siren still rang or had run out (then Duration is 0: its time was
	// said when it ran out).
	Ended bool
}

// AuthFailed: a wrong code was entered.
type AuthFailed struct {
	Origin      Origin
	Attempts    int       // wrong codes of this origin within LockoutWindow, this one included
	LockedUntil time.Time // zero unless this failure locked disarming from this origin
}

// ArmingScheduled: the system announces that it will arm by itself.
type ArmingScheduled struct {
	ScheduledFor time.Time
}

// Postponed: the automatic arming was pushed back.
type Postponed struct {
	ScheduledFor time.Time
	Count        int // postponements used for this closing
	Max          int // postponements allowed
}

func (ArmingScheduled) isEvent()   {}
func (Postponed) isEvent()         {}
func (ArmingStarted) isEvent()     {}
func (Armed) isEvent()             {}
func (Disarmed) isEvent()          {}
func (EntryDelayStarted) isEvent() {}
func (Triggered) isEvent()         {}
func (SirenCycleStarted) isEvent() {}
func (Cleared) isEvent()           {}
func (AuthFailed) isEvent()        {}
