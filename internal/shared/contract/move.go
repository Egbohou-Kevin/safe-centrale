package contract

import (
	"fmt"
	"net/url"
	"regexp"
	"time"
)

// Moving a site to another cell of the cloud (AD-20 of the cloud). The cloud of cell
// A orders the agent to move (agent.move on agent/cmd): the agent freezes what goes
// up, writes the new configuration beside the old one, and connects to cell B with
// the same identity and a new secret. B accepts: the new configuration becomes the
// current one, the queue goes to B. B unreachable past give_up_after: the agent goes
// back to A. Either way it says so (agent.moved on agent/moved).
//
// The order carries a secret, unlike any other order of agent/cmd: the cloud gives it
// a short MQTT expiry and until, never retains it, and sends it only to a connected
// hub (docs/contract.md).

// Bounds of a move, in seconds: how long the agent keeps trying cell B.
const (
	DefaultMoveGiveUp = 600
	MinMoveGiveUp     = 60
	MaxMoveGiveUp     = 3600
	MaxMoveURLs       = 4
)

// What became of a move (agent.moved).
const (
	MovedToB        = "b"    // cell B accepted the agent: it stays there
	MovedBack       = "back" // the agent went back to cell A
	MoveUnreachable = "unreachable"
	MoveNoSave      = "not_saved" // the new configuration could not be written
)

var (
	movedTo      = []string{MovedToB, MovedBack}
	moveReasons  = []string{MoveUnreachable, MoveNoSave}
	secretFormat = regexp.MustCompile(`^[!-~]{16,256}$`) // printable, no space: a file trims spaces
)

// MoveData: agent.move, from the cloud on agent/cmd, for the agent itself.
type MoveData struct {
	URLs   []string  `json:"urls"`   // cell B's broker, the first then the fallbacks (as cloud.urls)
	Secret string    `json:"secret"` // the site's new secret for cell B; never logged
	Until  Timestamp `json:"until"`  // an order seen after until is refused
	// GiveUpAfter is how long, in seconds, the agent keeps trying B before going back
	// to A; 0 for DefaultMoveGiveUp.
	GiveUpAfter int `json:"give_up_after,omitempty"`
}

// String hides the secret.
func (d MoveData) String() string {
	return fmt.Sprintf("{urls:%v secret:*** until:%s give_up_after:%d}", d.URLs, d.Until.Format(time.RFC3339), d.GiveUpAfter)
}

// GoString hides the secret for %#v.
func (d MoveData) GoString() string { return d.String() }

// Validate checks the order. Its errors never quote the secret nor a URL's
// credentials: they end up in the logs.
func (d MoveData) Validate() error {
	switch {
	case len(d.URLs) == 0 || len(d.URLs) > MaxMoveURLs:
		return fmt.Errorf("%w: from 1 to %d urls", ErrInvalid, MaxMoveURLs)
	case !secretFormat.MatchString(d.Secret):
		return fmt.Errorf("%w: the secret must be 16 to 256 printable characters, no space", ErrInvalid)
	case d.Until.IsZero():
		return fmt.Errorf("%w: until is required", ErrInvalid)
	case d.GiveUpAfter != 0 && (d.GiveUpAfter < MinMoveGiveUp || d.GiveUpAfter > MaxMoveGiveUp):
		return fmt.Errorf("%w: give_up_after must be from %d to %d seconds", ErrInvalid, MinMoveGiveUp, MaxMoveGiveUp)
	}
	for i, raw := range d.URLs {
		u, err := url.Parse(raw)
		switch {
		case err != nil || u.Host == "" || u.Hostname() == "":
			return fmt.Errorf("%w: urls[%d] is not an address", ErrInvalid, i)
		case u.Scheme != "mqtts" && u.Scheme != "wss":
			return fmt.Errorf("%w: urls[%d] is not encrypted (mqtts or wss)", ErrInvalid, i)
		case u.User != nil:
			return fmt.Errorf("%w: urls[%d] carries credentials", ErrInvalid, i)
		}
	}
	return nil
}

// GiveUp is how long the agent keeps trying cell B.
func (d MoveData) GiveUp() time.Duration {
	if d.GiveUpAfter == 0 {
		return DefaultMoveGiveUp * time.Second
	}
	return time.Duration(d.GiveUpAfter) * time.Second
}

// MovedData: agent.moved, from the agent on agent/moved: where a move ended. To B, it
// goes to B; back, to A.
type MovedData struct {
	Ref    string `json:"ref"`              // the agent.move
	To     string `json:"to"`               // MovedToB or MovedBack
	Reason string `json:"reason,omitempty"` // why it went back: MoveUnreachable or MoveNoSave
}

// Validate checks the report: a reason for a way back, none for B.
func (d MovedData) Validate() error {
	switch {
	case d.Ref == "":
		return fmt.Errorf("%w: ref is required", ErrInvalid)
	case d.To == MovedToB && d.Reason != "":
		return fmt.Errorf("%w: a move to B has no reason", ErrInvalid)
	case d.To == MovedBack && (d.Reason != MoveUnreachable && d.Reason != MoveNoSave):
		return fmt.Errorf("%w: a way back says why (%v)", ErrInvalid, moveReasons)
	case d.To != MovedToB && d.To != MovedBack:
		return fmt.Errorf("%w: to must be one of %v", ErrInvalid, movedTo)
	}
	return nil
}
