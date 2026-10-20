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
// in the reports.
type Request struct {
	Now      time.Time
	ID       string
	Incident string
	Contact  string
	Channel  Channel
	Priority Priority
	Text     string
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

// Action is what the gateway asks the driver to do.
type Action interface{ isAction() }

// Execute asks a line to send an SMS or ring a number. The line answers with an
// Outcome for Job.
type Execute struct {
	Job     uint64
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

func (Execute) isAction()      {}
func (CheckLine) isAction()    {}
func (RestartModem) isAction() {}
func (Report) isAction()       {}
func (Acknowledge) isAction()  {}
func (LinesChanged) isAction() {}
func (Ignored) isAction()      {}

// LineStatus is the public state of a line.
type LineStatus struct {
	ID         string
	Operator   string
	Responding bool
	Registered bool
	Signal     int    // rounded to tens: the exact value changes all the time
	Credit     string // "ok", "empty" (an operator said so), "" unknown
	LastTest   time.Time
	LastTestOK *bool
	Restarts   int
}

// Usable reports whether the line can send.
func (l LineStatus) Usable() bool { return l.Responding && l.Registered }
