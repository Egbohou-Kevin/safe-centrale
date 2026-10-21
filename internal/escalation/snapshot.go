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
	// Recent holds when each contact was last sent a message, for the hourly limit.
	Recent map[string][]time.Time `json:"recent"`
}

// IncidentSnap is the state of one alert being escalated.
type IncidentSnap struct {
	ID       string    `json:"id"`
	Kind     AlertKind `json:"kind"`
	Urgency  Urgency   `json:"urgency"`
	Devices  []string  `json:"devices"`
	Percent  int       `json:"percent"`
	RaisedAt time.Time `json:"raised_at"`

	Step         int                 `json:"step"`          // steps already run
	NextAt       time.Time           `json:"next_at"`       // when the next step is due, zero if none
	WaveContacts []string            `json:"wave_contacts"` // who the last step told
	Rounds       map[string]Round    `json:"rounds"`        // contact -> latest round of messages
	Deliveries   map[uint64]Delivery `json:"deliveries"`
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
	return out
}

func (in IncidentSnap) clone() IncidentSnap {
	in.Devices = slices.Clone(in.Devices)
	in.WaveContacts = slices.Clone(in.WaveContacts)
	rounds := make(map[string]Round, len(in.Rounds))
	for c, r := range in.Rounds {
		r.Sent = slices.Clone(r.Sent)
		rounds[c] = r
	}
	in.Rounds = rounds
	in.Deliveries = maps.Clone(in.Deliveries)
	if in.Deliveries == nil {
		in.Deliveries = map[uint64]Delivery{}
	}
	return in
}
