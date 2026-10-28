package engine

import (
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
	ReturnTo  Mode      `json:"return_to"` // mode to go back to when the incident ends: disarmed, arming or armed
}

// Schedule is the automatic arming planned for the next closing time. It exists
// only while the system is disarmed, and during the exit delay that it started (so
// that this delay can still be postponed).
type Schedule struct {
	// Closing is the closing time the plan is for, before any postponement. Boot
	// checks it against the configuration, which a restart may have changed.
	Closing   time.Time `json:"closing,omitzero"`
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
	// ArmedSince is when the exit delay of the current arming ended: the site stays
	// armed since then through an entry delay and an alarm that leave it armed (see
	// endIncident), which State says. Since stays when the mode began, which Boot
	// needs (see older). Zero while disarmed or arming.
	ArmedSince time.Time `json:"armed_since,omitzero"`

	Bypassed   map[string]bool `json:"bypassed"`    // devices bypassed until disarming
	Open       map[string]bool `json:"open"`        // contacts that are currently open
	EntryCause *Cause          `json:"entry_cause"` // what started the entry delay
	Incident   *Incident       `json:"incident"`
	// EntryEnds is the end of an entry delay that another alarm (a tamper, a fire, a
	// panic that sounds the siren) interrupted: EntryCause stays, and unless a code
	// ends that alarm first, the entry becomes an intrusion then (see lateEntry).
	EntryEnds time.Time `json:"entry_ends,omitzero"`
	// TimedOut are the incidents of this arming (intrusions, tampers) that no longer
	// ring and that nobody ended: their siren ran out, or they were raised while
	// another alarm rang. Their escalation goes on: the disarm that ends this arming
	// ends them too.
	TimedOut []string `json:"timed_out,omitempty"`
	// Raised are the other kinds of alarm raised while the current one rings that
	// neither wait for the siren nor alert per device (an intrusion), each alerted once
	// under its own incident (see during).
	Raised []AlertKind `json:"raised,omitempty"`
	// Waiting are the fires (and panics that sound the siren) raised while another
	// alarm rang, one entry per device still in alarm, the devices of a kind sharing
	// the incident of its alert: they ring in turn once it is over, if one of their
	// devices is still in alarm (see waitFor and wake).
	Waiting []Incident `json:"waiting,omitempty"`
	// Sounding are the devices of the fire (or the panic that sounds the siren) that
	// rings still in alarm: its cause, and those of its kind set off while it rings.
	// Another device of its kind joins it while one of them is; once all went back to
	// rest, one set off again is a new fire, which alerts and waits its turn (see
	// joins).
	Sounding []Cause `json:"sounding,omitempty"`
	// Checking are the orders whose code is being checked, with the time it was typed:
	// the end of an entry delay waits for them (see CodeEntered). A restart drops them.
	Checking map[string]time.Time `json:"checking,omitempty"`

	// Devices whose tamper is active and was reported, so that a restart does not
	// report it again.
	Tampered map[string]bool `json:"tampered"`
	// Heard is, per sensor ("dev_door/contact"), when the engine took the last state of
	// a door or a window, a smoke detector, a tamper or a panic button: Boot gives no
	// credit to a retained state older than that, which the broker may give back after
	// an unclean stop (see older). Open and Tampered move with each change anyway; a
	// panic button has neither, Heard is its only memory across a restart.
	Heard map[string]time.Time `json:"heard,omitempty"`

	// Memory of the last alarm, kept after the system goes back to armed.
	LastTrigger   *Cause    `json:"last_trigger,omitempty"`
	LastTriggerAt time.Time `json:"last_trigger_at,omitzero"`

	Schedule *Schedule `json:"schedule,omitempty"`

	// Failures are the wrong codes of the last LockoutWindow, oldest first, from the
	// keypad and the app alike: the lockout is the site's.
	Failures    []time.Time `json:"failures,omitempty"`
	LockedUntil time.Time   `json:"locked_until"` // disarming is refused until this time
	// WrongCodes counts the wrong codes since the last good one, however spread out,
	// from the keypad and the app alike: every LockoutAttempts of them alert the
	// contacts, locked or not. A lockout does not start it over.
	WrongCodes int `json:"wrong_codes,omitempty"`
}

func (s Snapshot) clone() Snapshot {
	s.Bypassed = maps.Clone(s.Bypassed)
	s.Open = maps.Clone(s.Open)
	s.Tampered = maps.Clone(s.Tampered)
	s.Heard = maps.Clone(s.Heard)
	s.Failures = slices.Clone(s.Failures)
	s.TimedOut = slices.Clone(s.TimedOut)
	s.Raised = slices.Clone(s.Raised)
	s.Waiting = slices.Clone(s.Waiting)
	s.Sounding = slices.Clone(s.Sounding)
	s.Checking = maps.Clone(s.Checking)
	s.EntryCause = clonePtr(s.EntryCause)
	s.Incident = clonePtr(s.Incident)
	s.LastTrigger = clonePtr(s.LastTrigger)
	s.Schedule = clonePtr(s.Schedule)
	return s
}

// clonePtr copies the value behind p, so the copy shares nothing with the original.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func newSnapshot(now time.Time) Snapshot {
	return Snapshot{Mode: ModeDisarmed, Since: now, Bypassed: map[string]bool{}, Open: map[string]bool{}, Tampered: map[string]bool{}}
}

// State is the public summary of the engine, meant to be published.
type State struct {
	Mode  Mode
	Since time.Time
	// EndsAt is the end of the exit or entry delay, or while triggered the end of the
	// siren cycle or of the pause under way; zero otherwise.
	EndsAt time.Time
	// While triggered: the incident that is ringing, and where its siren schedule
	// stands. SirenOn is the schedule, not the siren itself: false during the pause
	// between two cycles, and true for a silent siren too (the engine knows nothing
	// of it).
	Incident    string
	SirenOn     bool
	Cycle       int       // current siren cycle, starting at 1
	Of          int       // number of siren cycles
	LockedUntil time.Time // zero unless disarming is locked

	NextScheduled time.Time // next automatic arming while disarmed, zero if none
}
