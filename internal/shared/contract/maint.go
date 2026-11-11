package contract

import (
	"fmt"
	"slices"
)

// Remote maintenance (see docs/maintenance.md). When we need to work on a hub, the
// cloud asks it to open a tunnel (maint.open): the hub connects out to our bastion
// over SSH and offers its own SSH server there, for an hour at most. Nothing is opened
// on the client's box. Logging in still takes our administrators' key, which the
// cloud never holds. Every step is reported (maint.session), and the owner sees it in
// the app.

// MaxMaintLength is the longest a tunnel stays open, in seconds.
const MaxMaintLength = 3600

// Statuses of a maintenance session.
const (
	MaintOpening = "opening"
	MaintOpen    = "open"
	MaintClosed  = "closed"
	MaintFailed  = "failed"
)

// Reasons for the end or the failure of a session.
const (
	MaintStopped     = "stopped"     // asked by the cloud (maint.close)
	MaintTimeout     = "timeout"     // its length is over
	MaintBusy        = "busy"        // another session is open
	MaintExpired     = "expired"     // the order arrived past its exp
	MaintDisabled    = "disabled"    // remote maintenance is switched off on this hub
	MaintUnreachable = "unreachable" // the bastion did not let the hub in
)

var (
	maintStatuses = []string{MaintOpening, MaintOpen, MaintClosed, MaintFailed}
	maintReasons  = []string{MaintStopped, MaintTimeout, MaintBusy, MaintExpired, MaintDisabled, MaintUnreachable}
)

// MaintOpenData: maint.open, from the cloud on maint/cmd.
type MaintOpenData struct {
	Session   string    `json:"session"`    // chosen by the cloud
	User      string    `json:"user"`       // the administrator who asked: shown to the owner
	Reason    string    `json:"reason"`     // why, in a few words: shown to the owner
	DurationS int       `json:"duration_s"` // from 60 to MaxMaintLength
	Exp       Timestamp `json:"exp"`        // an order seen after exp is refused
}

// Validate checks the order.
func (d MaintOpenData) Validate() error {
	switch {
	case d.Session == "" || d.User == "":
		return fmt.Errorf("%w: session and user are required", ErrInvalid)
	case d.Reason == "" || len(d.Reason) > 200:
		return fmt.Errorf("%w: a reason of 200 characters at most is required", ErrInvalid)
	case d.DurationS < 60 || d.DurationS > MaxMaintLength:
		return fmt.Errorf("%w: duration_s must be from 60 to %d", ErrInvalid, MaxMaintLength)
	case d.Exp.IsZero():
		return fmt.Errorf("%w: exp is required", ErrInvalid)
	}
	return nil
}

// MaintCloseData: maint.close, from the cloud on maint/cmd.
type MaintCloseData struct {
	Session string `json:"session"`
}

// Validate checks the order.
func (d MaintCloseData) Validate() error {
	if d.Session == "" {
		return fmt.Errorf("%w: empty session", ErrInvalid)
	}
	return nil
}

// MaintSessionData: maint.session, from the hub on maint/session, at each step.
type MaintSessionData struct {
	Session   string     `json:"session"`
	User      string     `json:"user,omitempty"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason,omitempty"`     // for closed and failed
	Until     *Timestamp `json:"until,omitempty"`      // for opening and open: when it closes by itself
	DurationS int        `json:"duration_s,omitempty"` // for closed: how long it was open
}

// Validate checks the report.
func (d MaintSessionData) Validate() error {
	switch {
	case d.Session == "":
		return fmt.Errorf("%w: empty session", ErrInvalid)
	case !slices.Contains(maintStatuses, d.Status):
		return fmt.Errorf("%w: unknown maintenance status %q", ErrInvalid, d.Status)
	case d.Reason != "" && !slices.Contains(maintReasons, d.Reason):
		return fmt.Errorf("%w: unknown maintenance reason %q", ErrInvalid, d.Reason)
	}
	return nil
}
