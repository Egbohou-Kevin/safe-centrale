package escalation

import (
	"maps"
	"slices"
	"time"
)

// Snapshot is the full state, saved by the driver after every change so that a
// restart goes on with the incidents in progress instead of forgetting them.
type Snapshot struct {
	NextKey    uint64                  `json:"next_key"`
	UplinkDown bool                    `json:"uplink_down"`
	Incidents  map[string]IncidentSnap `json:"incidents"` // keyed by incident and kind
	// Recent holds when each contact was last sent a message, for the hourly limit;
	// Lesser those of them for alerts below an intrusion, which take half of it at
	// most (see allow).
	Recent map[string][]time.Time `json:"recent"`
	Lesser map[string][]time.Time `json:"lesser,omitempty"`
	// Handled holds the incidents a contact acknowledged or a person stopped with a
	// code, for a day: the escalation forgets an incident once it is over, but an
	// alert that names it as its alarm must still know it was handled.
	Handled map[string]HandledAlarm `json:"handled,omitempty"`
	// SirenReported holds, for a day, when each silent siren was reported for each
	// alarm ("alarm/siren"): a siren is reported once per alarm, after a restart too.
	SirenReported map[string]time.Time `json:"siren_reported,omitempty"`
}

// HandledAlarm is how an incident was handled. Closed: a person stopped it with a
// code (a disarm, or the siren stopped), rather than a contact acknowledging it.
type HandledAlarm struct {
	At     time.Time `json:"at"`
	Closed bool      `json:"closed,omitempty"`
}

// IncidentSnap is the state of one alert being escalated.
type IncidentSnap struct {
	ID       string    `json:"id"`
	Kind     AlertKind `json:"kind"`
	Urgency  Urgency   `json:"urgency"`
	Devices  []string  `json:"devices"`
	Percent  int       `json:"percent"`
	RaisedAt time.Time `json:"raised_at"`
	// Alarm is the alarm under way the alert is part of (Raise.Alarm). Quiet: that
	// alarm is handled, the alert is told without ringing (see quieten).
	Alarm string `json:"alarm,omitempty"`
	Quiet bool   `json:"quiet,omitempty"`
	// Silent: the sirens known not to sound as the alert was raised (Raise.Silent).
	Silent []string `json:"silent,omitempty"`

	Step         int                 `json:"step"`          // steps already run
	NextAt       time.Time           `json:"next_at"`       // when the next step is due, zero if none
	WaveContacts []string            `json:"wave_contacts"` // who the last step told
	Rounds       map[string]Round    `json:"rounds"`        // contact -> latest round of messages
	Deliveries   map[uint64]Delivery `json:"deliveries"`
	// Seen: the contacts who said they have seen it ("Vu"), and when. They are told
	// nothing more of it; the others are (see Seen).
	Seen map[string]time.Time `json:"seen,omitempty"`
}

// Round is the messages of one step to one contact, and the timers that go with
// them.
type Round struct {
	Step    int       `json:"step"`
	Started time.Time `json:"started"`
	Sent    []Channel `json:"sent"` // the channels used in this round
	NetOK   bool      `json:"net_ok"`
	// NetHeld: the internet message was held back because the uplink was down.
	NetHeld     bool      `json:"net_held"`
	NetDeadline time.Time `json:"net_deadline"` // the cloud must have confirmed by then
	GSMDue      time.Time `json:"gsm_due"`      // when the GSM channels take over or double
}

// Delivery is one message sent, for matching its result.
type Delivery struct {
	// Ref is the id of the request that carries the message (see Bind): saved with
	// the rest, so a result still finds its message after a restart.
	Ref     string       `json:"ref,omitempty"`
	Contact string       `json:"contact"`
	Channel Channel      `json:"channel"`
	Step    int          `json:"step"`
	Status  ResultStatus `json:"status"`
}

func (s Snapshot) clone() Snapshot {
	out := Snapshot{NextKey: s.NextKey, UplinkDown: s.UplinkDown}
	out.Incidents = make(map[string]IncidentSnap, len(s.Incidents))
	for k, in := range s.Incidents {
		out.Incidents[k] = in.clone()
	}
	out.Recent = make(map[string][]time.Time, len(s.Recent))
	for c, ts := range s.Recent {
		out.Recent[c] = slices.Clone(ts)
	}
	out.Lesser = make(map[string][]time.Time, len(s.Lesser))
	for c, ts := range s.Lesser {
		out.Lesser[c] = slices.Clone(ts)
	}
	// A state saved before these maps existed has none: they are made here, so the
	// first write after Restore does not panic.
	out.Handled = maps.Clone(s.Handled)
	if out.Handled == nil {
		out.Handled = map[string]HandledAlarm{}
	}
	out.SirenReported = maps.Clone(s.SirenReported)
	if out.SirenReported == nil {
		out.SirenReported = map[string]time.Time{}
	}
	return out
}

func (in IncidentSnap) clone() IncidentSnap {
	in.Devices = slices.Clone(in.Devices)
	in.Silent = slices.Clone(in.Silent)
	in.WaveContacts = slices.Clone(in.WaveContacts)
	rounds := make(map[string]Round, len(in.Rounds))
	for c, r := range in.Rounds {
		r.Sent = slices.Clone(r.Sent)
		rounds[c] = r
	}
	in.Rounds = rounds
	in.Seen = maps.Clone(in.Seen)
	in.Deliveries = maps.Clone(in.Deliveries)
	if in.Deliveries == nil {
		in.Deliveries = map[uint64]Delivery{}
	}
	return in
}
