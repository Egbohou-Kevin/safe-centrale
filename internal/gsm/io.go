package gsm

import "time"

// Channel is how a contact is reached through the modem.
type Channel string

const (
	ChannelSMS  Channel = "sms"
	ChannelCall Channel = "call"
)

// Priority orders the queue: what protects lives leaves first.
type Priority int

const (
	PriorityLifeSafety Priority = iota // fire, panic
	PriorityIntrusion
	PriorityOther
)

// Reason explains a failure.
type Reason string

const (
	ReasonNoNetwork   Reason = "no_network"
	ReasonNoCredit    Reason = "no_credit"
	ReasonModemError  Reason = "modem_error"
	ReasonTimeout     Reason = "timeout"
	ReasonBusy        Reason = "busy"
	ReasonUnreachable Reason = "unreachable"
	// ReasonDropped: its incident ended before the message left (a person ended
	// the alarm, or a contact handles it), so it was not sent.
	ReasonDropped Reason = "dropped"
)

// Status is what is reported about a request.
type Status string

const (
	StatusQueued    Status = "queued"
	StatusSent      Status = "sent"
	StatusDelivered Status = "delivered"
	StatusFailed    Status = "failed"
)

// Input is what the gateway receives. Every input carries its time; the package
// never reads the clock.
type Input interface{ at() time.Time }

// Tick advances the clock. The driver sends one at the time returned by NextWake.
type Tick struct{ Now time.Time }

// Request asks for a message to a contact. ID is the request's identifier, echoed
// in the reports. Asked is when the escalation asked (the request's own time,
// zero for Now): the maximum age counts from there, so a request held by the
// broker while the gateway was down does not go out late.
type Request struct {
	Now      time.Time
	Asked    time.Time
	ID       string
	Incident string
	Contact  string
	Channel  Channel
	Priority Priority
	// Secondary marks an alert that belongs to an alarm told alongside it, to the same
	// people (a silent siren): acknowledging it ends only it, less than the alarm, so
	// a "1" that may be meant for either ends it first (see ackTarget).
	Secondary bool
	Text      string
}

// Outcome is what a line did with an Execute. MsgRef is the operator's reference
// of an SMS, used to match its delivery report.
type Outcome struct {
	Now    time.Time
	Job    uint64
	Line   string
	OK     bool
	Reason Reason
	MsgRef string
}

// DeliveryReport is the operator's confirmation that an SMS reached the phone, or
// did not.
type DeliveryReport struct {
	Now    time.Time
	Line   string
	MsgRef string
	OK     bool
}

// Incoming is an SMS received on a line.
type Incoming struct {
	Now  time.Time
	Line string
	From string // as the modem gives it
	Text string
	// Sent is when the operator's service centre took the SMS (its time stamp), zero
	// if unknown: a reply held by the network, or stored while the gateway was down,
	// may be read long after (see incoming).
	Sent time.Time
}

// Probe is the answer to a CheckLine.
type Probe struct {
	Now        time.Time
	Line       string
	Responding bool
	Registered bool
	Signal     int // 0 to 100
	Operator   string
}

// Cancel says that an incident is over: the alarm was disarmed, or a contact
// acknowledged it. Its messages still waiting are dropped, and so is any request
// of it that arrives later. One a line is sending goes on, but is not retried.
type Cancel struct {
	Now      time.Time
	Incident string
	// Confirm is the contact who acknowledged the incident in the app, if so: told by
	// SMS (see confirmText).
	Confirm string
}

// Restarted says that a RestartModem is over.
type Restarted struct {
	Now  time.Time
	Line string
}

func (t Tick) at() time.Time           { return t.Now }
func (r Request) at() time.Time        { return r.Now }
func (o Outcome) at() time.Time        { return o.Now }
func (d DeliveryReport) at() time.Time { return d.Now }
func (i Incoming) at() time.Time       { return i.Now }
func (p Probe) at() time.Time          { return p.Now }
func (r Restarted) at() time.Time      { return r.Now }
func (c Cancel) at() time.Time         { return c.Now }

// Action is what the gateway asks the driver to do.
type Action interface{ isAction() }

// Execute asks a line to send an SMS or ring a number. The line answers with an
// Outcome for Job.
type Execute struct {
	Job     uint64
	Ref     string // the request's id, empty for a test: for the logs
	Line    string
	Channel Channel
	Number  string
	Text    string
	RingFor time.Duration // for a call
}

// CheckLine asks a line whether its modem answers, and how the network looks. The
// line answers with a Probe.
type CheckLine struct{ Line string }

// RestartModem asks for the modem of a line to be restarted. The line answers with
// Restarted.
type RestartModem struct{ Line string }

// Report tells the escalation what became of a request.
type Report struct {
	Ref    string
	Status Status
	Reason Reason
}

// Acknowledge is a contact who answered "1": it ends the escalation of the incident.
type Acknowledge struct {
	Incident string
	Contact  string
}

// LinesChanged gives the public state of every line, when it changed.
type LinesChanged struct{ Lines []LineStatus }

// Ignored reports an incoming SMS the gateway did nothing with.
type Ignored struct {
	Line string
	Why  string // "unknown number", "unknown text", "nothing to acknowledge"
}

// Dropped reports the messages of an incident given up because it is over. Nothing
// is reported to the escalation: it stopped waiting for them.
type Dropped struct {
	Incident string
	Jobs     int
}

func (Execute) isAction()      {}
func (CheckLine) isAction()    {}
func (RestartModem) isAction() {}
func (Report) isAction()       {}
func (Acknowledge) isAction()  {}
func (LinesChanged) isAction() {}
func (Ignored) isAction()      {}
func (Dropped) isAction()      {}

// LineStatus is the public state of a line.
type LineStatus struct {
	ID         string
	Operator   string
	Responding bool
	Registered bool
	Signal     int // rounded to tens: the exact value changes all the time
	Credit     Credit
	LastTest   time.Time
	LastTestOK *bool
	Restarts   int
}

// Credit is what the gateway knows of a line's credit; empty when unknown.
type Credit string

const (
	CreditOK    Credit = "ok"
	CreditEmpty Credit = "empty" // an operator said so
)
