package contract

import "fmt"

// Numbering and replay of what reaches the cloud (see docs/agent.md). Each program of
// the site numbers its own messages (seq), and many never leave the site: in the
// cloud, those numbers would have holes for no reason. So the link agent numbers
// again what it sends to the cloud, at the first attempt to send it: there, seq is
// continuous per site, and a hole means a message the cloud missed. The cloud may
// then ask for it again (agent.replay); the agent sends again what it still holds,
// and says what it no longer has (agent.replayed).

// MaxReplay is how many numbers one agent.replay may ask for.
const MaxReplay = 1000

// ReplayData: agent.replay, from the cloud on agent/cmd, for the agent itself: send
// again the messages numbered From to To, both included.
type ReplayData struct {
	From uint64 `json:"from"`
	To   uint64 `json:"to"`
}

// Validate checks the request.
func (d ReplayData) Validate() error {
	switch {
	case d.From < 1 || d.To < d.From:
		return fmt.Errorf("%w: from must be at least 1 and to at least from", ErrInvalid)
	case d.To-d.From >= MaxReplay:
		return fmt.Errorf("%w: at most %d messages per replay", ErrInvalid, MaxReplay)
	}
	return nil
}

// SeqRange is a run of numbers, both ends included.
type SeqRange struct {
	First uint64 `json:"first"`
	Last  uint64 `json:"last"`
}

// ReplayedData: agent.replayed, from the agent on agent/replayed: how a replay went.
// The messages it sends again keep their id and their seq, and are never retained.
type ReplayedData struct {
	From    uint64     `json:"from"`
	To      uint64     `json:"to"`
	Last    uint64     `json:"last"`              // the highest number given so far
	Resent  int        `json:"resent"`            // messages queued to go again
	Missing []SeqRange `json:"missing,omitempty"` // numbers the agent does not hold (any more)
}

// Validate checks the report.
func (d ReplayedData) Validate() error {
	if err := (ReplayData{From: d.From, To: d.To}).Validate(); err != nil {
		return err
	}
	if d.Resent < 0 {
		return fmt.Errorf("%w: resent must not be negative", ErrInvalid)
	}
	for _, r := range d.Missing {
		if r.First < d.From || r.Last < r.First || r.Last > d.To {
			return fmt.Errorf("%w: a missing range is outside the replay", ErrInvalid)
		}
	}
	return nil
}
