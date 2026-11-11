package maint

import "time"

// Input is what the service is told. Every input carries the time it happened.
type Input interface{ at() time.Time }

// Open asks for a session (maint.open).
type Open struct {
	Now     time.Time
	Session string
	User    string
	Length  time.Duration // asked by the cloud
	Exp     time.Time     // an order seen after Exp is refused
}

// Close asks to end a session (maint.close).
type Close struct {
	Now     time.Time
	Session string
}

// Up says the bastion accepted the tunnel.
type Up struct {
	Now     time.Time
	Session string
}

// Down says the tunnel stopped by itself.
type Down struct {
	Now     time.Time
	Session string
}

// Boot says the service started.
type Boot struct{ Now time.Time }

// Tick says time has passed.
type Tick struct{ Now time.Time }

func (i Open) at() time.Time  { return i.Now }
func (i Close) at() time.Time { return i.Now }
func (i Up) at() time.Time    { return i.Now }
func (i Down) at() time.Time  { return i.Now }
func (i Boot) at() time.Time  { return i.Now }
func (i Tick) at() time.Time  { return i.Now }

// Action is what the core asks the application to do.
type Action interface{ isAction() }

// Start: open the tunnel to the bastion.
type Start struct{ Session string }

// Stop: close it.
type Stop struct{ Session string }

// Report: tell the cloud, and so the owner, how the session goes (maint.session).
type Report struct {
	Session  string
	User     string
	Status   Status
	Reason   Reason
	Until    time.Time     // for Opening and IsOpen
	Duration time.Duration // for Closed: how long it was open
}

func (Start) isAction()  {}
func (Stop) isAction()   {}
func (Report) isAction() {}

// Status is the step of a session.
type Status string

const (
	Opening Status = "opening"
	IsOpen  Status = "open"
	Closed  Status = "closed"
	Failed  Status = "failed"
)

// Reason says why a session ended or failed.
type Reason string

const (
	Stopped     Reason = "stopped"
	Timeout     Reason = "timeout"
	Busy        Reason = "busy"
	Expired     Reason = "expired"
	Disabled    Reason = "disabled"
	Unreachable Reason = "unreachable"
)
