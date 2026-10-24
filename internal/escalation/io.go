package escalation

import "time"

// Role is the part a contact plays on a site. It decides when the contact is told.
type Role string

const (
	RoleOwner   Role = "owner"
	RoleDeputy  Role = "deputy" // the owner's delegate, optional
	RoleGuard   Role = "guard"  // optional
	RoleManager Role = "manager"
	RoleOther   Role = "other"
)

func (r Role) valid() bool {
	switch r {
	case RoleOwner, RoleDeputy, RoleGuard, RoleManager, RoleOther:
		return true
	}
	return false
}

// Channel is a way to reach a contact.
type Channel string

const (
	ChannelApp      Channel = "app"
	ChannelWhatsApp Channel = "whatsapp"
	ChannelSMS      Channel = "sms"
	ChannelCall     Channel = "call"
)

func (c Channel) valid() bool { return c.internet() || c.gsm() }

// internet channels go through the cloud; gsm channels go through the hub's modem
// and work without internet.
func (c Channel) internet() bool { return c == ChannelApp || c == ChannelWhatsApp }
func (c Channel) gsm() bool      { return c == ChannelSMS || c == ChannelCall }

// AlertKind is the type of problem an alert reports.
type AlertKind string

const (
	KindIntrusion      AlertKind = "intrusion"
	KindFire           AlertKind = "fire"
	KindPanic          AlertKind = "panic"
	KindTamper         AlertKind = "tamper"
	KindAuthFailed     AlertKind = "auth_failed"
	KindArmingBypassed AlertKind = "arming_bypassed"
	KindHubBatteryLow  AlertKind = "hub_battery_low"
	KindMainsRestored  AlertKind = "mains_restored"
	KindDeviceOffline  AlertKind = "device_offline"
	KindBatteryLow     AlertKind = "battery_low"
	KindGSMDegraded    AlertKind = "gsm_degraded"
	KindZigbeeDown     AlertKind = "zigbee_down"
	KindSirenFailed    AlertKind = "siren_failed"
)

var allKinds = []AlertKind{
	KindIntrusion, KindFire, KindPanic, KindTamper, KindAuthFailed, KindArmingBypassed,
	KindHubBatteryLow, KindMainsRestored, KindDeviceOffline, KindBatteryLow, KindGSMDegraded, KindZigbeeDown,
	KindSirenFailed,
}

// Urgency ranks how fast someone must react to an alert.
type Urgency string

const (
	UrgencyCritical Urgency = "critical"
	UrgencyHigh     Urgency = "high"
	UrgencyLow      Urgency = "low"
)

// ResultStatus is what a channel reports about one message.
type ResultStatus string

const (
	ResultQueued    ResultStatus = "queued"
	ResultSent      ResultStatus = "sent"
	ResultDelivered ResultStatus = "delivered"
	ResultFailed    ResultStatus = "failed"
)

// Input is what the escalation receives. Every input carries the time it happened;
// the package never reads the clock.
type Input interface{ at() time.Time }

// Tick advances the clock. The driver sends one at the time returned by NextWake.
type Tick struct{ Now time.Time }

// Raise reports a problem that someone must be told about. The same incident and
// kind raised again are merged into the one in progress: several detectors going
// off during one incident do not each send their own messages.
type Raise struct {
	Now      time.Time
	Incident string
	Kind     AlertKind
	Urgency  Urgency
	Devices  []string // devices concerned
	Percent  int      // battery level, when it applies
	// Alarm names the alarm under way this alert is part of, never Incident itself:
	// a siren that does not sound during an alarm. Once that alarm is handled, the
	// alert is told without ringing (see Send.AlarmHandled).
	Alarm string
}

// Result is a channel's report about a message sent with Send. Ref, when set, is
// the id of the request that carried it (see Escalation.Bind); otherwise Key
// identifies it.
type Result struct {
	Now    time.Time
	Key    uint64
	Ref    string
	Status ResultStatus
}

// Ack is a contact saying "I have seen it, I am dealing with it": it ends the
// escalation of the incident, which is handled from then on (see Raise.Alarm). It
// does not disarm the alarm or silence the siren.
type Ack struct {
	Now      time.Time
	Incident string
	Contact  string
}

// Uplink says whether the hub can reach the cloud, where the internet channels
// live.
type Uplink struct {
	Now time.Time
	Up  bool
}

// Close ends the escalation of an incident that a person ended with a code (the
// alarm was disarmed, or its siren stopped): nobody needs to be told any more, and
// the incident is handled (see Raise.Alarm).
type Close struct {
	Now      time.Time
	Incident string
}

func (t Tick) at() time.Time   { return t.Now }
func (r Raise) at() time.Time  { return r.Now }
func (r Result) at() time.Time { return r.Now }
func (a Ack) at() time.Time    { return a.Now }
func (u Uplink) at() time.Time { return u.Now }
func (c Close) at() time.Time  { return c.Now }

// Action is what the escalation asks the driver to do.
type Action interface{ isAction() }

// Send asks a channel to deliver a message to a contact. Key identifies the
// message: the channel's Result refers to it.
type Send struct {
	Key      uint64
	Incident string
	RaisedAt time.Time // when the incident was raised: the same on every wave and reminder
	Kind     AlertKind
	Urgency  Urgency
	Wave     int // 1 for the first people told, then growing
	Contact  string
	Channel  Channel
	Devices  []string
	Percent  int
	Text     string
	// Alarm is the alarm under way the alert is part of (Raise.Alarm), "" for most.
	Alarm string
	// AlarmHandled: that alarm is already handled, a contact acknowledged it or a
	// person stopped it with a code. The alert is told without ringing: never by
	// call, at urgency low, without a way to answer.
	AlarmHandled bool
}

// Throttled reports a message that was not sent because the contact already got the
// maximum per hour. Fire, panic and a silent siren are never throttled.
type Throttled struct {
	Incident string
	Contact  string
	Channel  Channel
}

// Withheld reports a call that was not placed because the alarm the alert is part
// of is already handled: such an alert is told without ringing. A contact who only
// accepts calls is not told at all, which the driver logs.
type Withheld struct {
	Incident string
	Alarm    string
	Contact  string
	Channel  Channel
}

// Outcome says how the escalation of an incident ended.
type Outcome string

const (
	OutcomeAcknowledged Outcome = "acknowledged" // a contact answered
	OutcomeUnanswered   Outcome = "unanswered"   // every step ran and nobody answered
	OutcomeClosed       Outcome = "closed"       // the incident ended by itself
	// OutcomeInformed: an alert of an alarm already handled said what it had to say;
	// it asked for no answer, so it is not "unanswered".
	OutcomeInformed Outcome = "informed"
)

// Concluded reports the end of an incident's escalation.
type Concluded struct {
	Incident string
	Kind     AlertKind
	Outcome  Outcome
	By       string // the contact, for OutcomeAcknowledged
}

func (Send) isAction()      {}
func (Throttled) isAction() {}
func (Withheld) isAction()  {}
func (Concluded) isAction() {}
