package zigbee

import "time"

// Input is what the network core receives. Every input carries the time it
// happened; the package never reads the clock.
type Input interface{ at() time.Time }

// Tick advances the clock. The driver sends one at the time returned by NextWake.
type Tick struct{ Now time.Time }

// Report is a message from a device, as Zigbee2MQTT publishes it: the whole known
// state of the device, field by field.
type Report struct {
	Now  time.Time
	Name string // the device's name in Zigbee2MQTT: our device id, or its IEEE address
	// Fields is the decoded message: bool, float64 or string values, maps and lists.
	Fields map[string]any
	// Retained is true for a message the broker kept from before: it restores a
	// state, and never makes an event (an old "door open" replayed at start must not
	// trip the alarm). It says nothing about the device being alive now.
	Retained bool
	// Heard is when Zigbee2MQTT heard the device (its last_seen), zero if it does not
	// say. A message may reach us much later: the broker keeps them while the adapter
	// is down.
	Heard time.Time
}

// Bridge says whether Zigbee2MQTT and its radio stick are up.
type Bridge struct {
	Now    time.Time
	Online bool
}

// Interviewed says that Zigbee2MQTT finished questioning a device that joined: OK
// with its model, or not OK when the model could not be read.
type Interviewed struct {
	Now   time.Time
	IEEE  string
	Model string
	OK    bool
}

// Left says that a device left the network.
type Left struct {
	Now  time.Time
	IEEE string
}

// Pair opens the network to new devices for For; zero or less closes it.
type Pair struct {
	Now time.Time
	Ref string
	For time.Duration
}

// JoinAnswer is Zigbee2MQTT's answer to a request to open the network: OK, or
// refused with its reason. Txn is the transaction the request carried, which
// Zigbee2MQTT echoes; empty when the answer has none.
type JoinAnswer struct {
	Now    time.Time
	Txn    string
	OK     bool
	Reason string
}

// Remove takes a device out of the network and the registry.
type Remove struct {
	Now      time.Time
	Ref      string
	DeviceID string
}

// Command is an order for a device: a command of its profile.
type Command struct {
	Now       time.Time
	Ref       string
	DeviceID  string
	Name      string
	DurationS int       // replaces "{duration_s}" in the profile's payload
	Exp       time.Time // zero: no expiry
}

func (t Tick) at() time.Time        { return t.Now }
func (r Report) at() time.Time      { return r.Now }
func (b Bridge) at() time.Time      { return b.Now }
func (i Interviewed) at() time.Time { return i.Now }
func (l Left) at() time.Time        { return l.Now }
func (p Pair) at() time.Time        { return p.Now }
func (j JoinAnswer) at() time.Time  { return j.Now }
func (r Remove) at() time.Time      { return r.Now }
func (c Command) at() time.Time     { return c.Now }

// Action is what the core asks the driver to do.
type Action interface{ isAction() }

// Changed is a new value of a capability.
type Changed struct {
	DeviceID   string
	Capability string
	Value      string // a number is written in decimal
	Numeric    bool
	Since      time.Time
	Notify     bool   // announce it (device.event)
	State      bool   // it is the device's new state (retained device.state)
	Code       string // the code typed with a keypad action; never logged
}

// Presence says that a device stopped answering, or answers again.
//
// Heard, for a device that stopped answering, is when it last spoke, to the minute:
// zero when it said nothing since the adapter started. It is the last news the owner
// had of it, fixed while it stays offline; a device that answers has none, since
// when it last spoke moves with every door and every passer-by.
type Presence struct {
	DeviceID string
	Online   bool
	Since    time.Time
	Heard    time.Time
}

// Send writes to a device through Zigbee2MQTT. The IEEE address names the device
// whatever its name in Zigbee2MQTT.
type Send struct {
	IEEE    string
	Payload map[string]any
}

// PermitJoin opens the network for Seconds, or closes it at zero. Txn, set on a
// request to open, is echoed in Zigbee2MQTT's answer (JoinAnswer): it ties the
// answer to its request.
type PermitJoin struct {
	Seconds int
	Txn     string
}

// Rename gives a new device its id as its name in Zigbee2MQTT: no maker's name in
// the topics and the logs.
type Rename struct{ IEEE, To string }

// Expel takes a device out of the Zigbee network.
type Expel struct{ IEEE string }

// CarriesCode says that a device's messages carry an access code (a keypad):
// Zigbee2MQTT must publish them at QoS 0, so that the broker never keeps one for a
// subscriber away.
type CarriesCode struct{ IEEE string }

// Ack answers a command.
type Ack struct {
	Ref      string
	DeviceID string
	Status   string // AckAccepted, AckDone, AckFailed, AckExpired
	Reason   string
}

// Added: a device was accepted at pairing.
type Added struct {
	Ref    string // the pairing order
	Device Device
}

// Rejected: a device that joined was refused and expelled.
type Rejected struct {
	Ref    string // the pairing order, empty outside a pairing window
	Model  string
	Reason string // RejectUnknownModel, RejectNotPairing, RejectInterviewFailed
}

// PairFailed: the network could not be opened (or kept open) for a pairing order;
// the window is over.
type PairFailed struct {
	Ref    string // the pairing order
	Reason string // Zigbee2MQTT's words, or ReasonNoJoinAnswer
}

// Removed: a device left the registry. Its retained states are to be cleared.
type Removed struct {
	Ref    string
	Device Device
}

// RegistryChanged gives the new list of devices.
type RegistryChanged struct{ Devices []Device }

// Ignored reports an input that changed nothing, for the logs.
type Ignored struct{ What string }

func (Changed) isAction()         {}
func (Presence) isAction()        {}
func (Send) isAction()            {}
func (PermitJoin) isAction()      {}
func (Rename) isAction()          {}
func (Expel) isAction()           {}
func (CarriesCode) isAction()     {}
func (Ack) isAction()             {}
func (Added) isAction()           {}
func (Rejected) isAction()        {}
func (PairFailed) isAction()      {}
func (Removed) isAction()         {}
func (RegistryChanged) isAction() {}
func (Ignored) isAction()         {}

// Command acknowledgement statuses and reasons.
const (
	AckAccepted = "accepted"
	AckDone     = "done"
	AckFailed   = "failed"
	AckExpired  = "expired"

	ReasonUnknownDevice  = "unknown_device"  // no such device in the registry
	ReasonUnsupported    = "unsupported"     // its profile has no such command
	ReasonNoConfirmation = "no_confirmation" // the device never confirmed, after every attempt
	ReasonSuperseded     = "superseded"      // a newer command for the same device came first
	ReasonRemoved        = "removed"         // the device was removed meanwhile
)

// ReasonNoJoinAnswer is why a pairing window failed when Zigbee2MQTT never answered
// the request to open the network.
const ReasonNoJoinAnswer = "Zigbee2MQTT did not answer the request to open the network: is it running?"

// Why a device was refused at pairing.
const (
	RejectUnknownModel    = "unknown_model"
	RejectNotPairing      = "not_pairing"
	RejectInterviewFailed = "interview_failed"
)
