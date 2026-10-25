package agent

import (
	"encoding/json"
	"time"

	"safehome/internal/contract"
)

// Input is what the relay is told. Every input carries the time it happened.
type Input interface{ at() time.Time }

// Local is a message read on the hub's broker.
type Local struct {
	Now     time.Time
	Topic   string
	Payload []byte
}

// Own is a message the agent itself publishes on the hub's broker (its status, a
// failed alert): the agent does not read its own publications back, so it hands
// them to the relay to go up like the others. A state of the hub said again
// (Restate), with its new id, comes back this way too.
type Own struct {
	Now     time.Time
	Topic   string
	Payload []byte
}

// Cloud is a message read on the cloud's broker.
type Cloud struct {
	Now time.Time
	// Wall is the system clock as it is when the message is taken, zero for Now. After
	// the clock was set back, Now stands still until the clock catches up (the driver's
	// guard): an expiry the cloud stamped on the corrected clock is judged on Wall.
	Wall    time.Time
	Topic   string
	Payload []byte
}

// Sent says the cloud's broker acknowledged an item.
type Sent struct {
	Now time.Time
	ID  string
}

// Rejected says the cloud's broker refused an item for good (not authorized,
// invalid): tried again, it would hold the queue behind it for days.
type Rejected struct {
	Now time.Time
	ID  string
}

// Link says the connection to the cloud came up or went down. Transport is the URL
// scheme in use ("mqtts", "wss").
type Link struct {
	Now       time.Time
	Up        bool
	Transport string
}

// Tick says time has passed.
type Tick struct{ Now time.Time }

func (i Local) at() time.Time    { return i.Now }
func (i Own) at() time.Time      { return i.Now }
func (i Cloud) at() time.Time    { return i.Now }
func (i Sent) at() time.Time     { return i.Now }
func (i Rejected) at() time.Time { return i.Now }
func (i Link) at() time.Time     { return i.Now }
func (i Tick) at() time.Time     { return i.Now }

// Action is what the relay asks the application to do.
type Action interface{ isAction() }

// PublishLocal: publish this message from the cloud on the hub's broker, as it came.
// Code says it may carry the site's access code (downRoute.code): it goes at QoS 0,
// so that the hub's broker keeps none for a hub away either.
type PublishLocal struct {
	Topic   string
	Payload []byte
	Code    bool
}

// FailAlert: tell the hub that an alert for a cloud channel will not get through
// (alert.result failed, no_uplink), so that its escalation turns to the GSM now.
type FailAlert struct {
	Ref string // the alert.request
}

// AnswerOrder: an order from the app the agent does not pass on to the hub (too late,
// or over the limit), answered to the cloud at once (alarm.ack, ref the order's id).
// It never reaches the hub.
type AnswerOrder struct {
	Ref    string // the alarm.command
	Status string // contract.AckExpired or contract.AckRefused
	Reason string // contract.ReasonRateLimited for a refusal
}

// StatusChanged: the link changed, publish agent.status.
type StatusChanged struct{ Status Status }

// Refused: a message from the cloud was refused, for the logs.
type Refused struct {
	Topic  string
	Reason string
}

// Late: the cloud's word on an alert the hub was already told failed (it turned to
// the GSM), dropped: it would disturb the escalation. For the logs.
type Late struct {
	Ref    string // the alert.request
	Status string
}

// Dropped: items left the queue without reaching the cloud (full, too old, refused
// by the cloud's broker).
type Dropped struct {
	Count  int
	Reason string
}

func (PublishLocal) isAction()  {}
func (FailAlert) isAction()     {}
func (AnswerOrder) isAction()   {}
func (StatusChanged) isAction() {}
func (Refused) isAction()       {}
func (Late) isAction()          {}
func (Dropped) isAction()       {}
func (Replayed) isAction()      {}

// Replayed: the cloud asked for messages again; publish agent.replayed.
type Replayed struct{ Report contract.ReplayedData }

// Priority orders the queue: what matters most leaves first.
type Priority int

const (
	PriorityState  Priority = iota // retained states and the heartbeat: only the latest counts
	PriorityNormal                 // device events, video sessions
	PriorityAlarm                  // alarm and alert messages
)

// Item is a message waiting for the cloud.
type Item struct {
	ID      string          `json:"id"` // the envelope's id
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload"`
	Prio    Priority        `json:"prio"`
	Retain  bool            `json:"retain"`
	// Key, when set, makes a newer item with the same key replace this one: a
	// retained state or the heartbeat, of which only the latest is worth sending.
	Key string    `json:"key,omitempty"`
	At  time.Time `json:"at"` // when it entered the queue
	// Seq is its number for the cloud, given at its first attempt to leave (also
	// written in Payload); 0 before.
	Seq uint64 `json:"seq,omitempty"`
	// Order is its place in the queue: at equal priority, items leave in this order.
	// It is saved with the item, so the order survives a restart.
	Order uint64 `json:"order"`
	// Own: the agent queued it itself (Own), its id is not the hub's. A state said
	// again is one: at a restart, it must not be taken for the hub's state (see
	// RestoreQueue).
	Own bool `json:"own,omitempty"`
	// Sent: a tombstone, kept on disk for a while once the item reached the cloud,
	// with only its id, topic, number and time (see sent.go). Not in the queue.
	Sent bool `json:"sent,omitempty"`
}

// Status is the state of the link, as agent.status says it.
type Status struct {
	Up        bool
	Transport string
	Queued    int
	Oldest    time.Duration
	Dropped   int
}
