package agent

import (
	"slices"
	"time"

	"safehome/internal/contract"
)

// Moving to another cell of the cloud (agent.move, see docs/agent.md). From the
// order on, nothing more goes up (the freeze): what comes stays in the queue on disk,
// in its order, numbered as before. The application writes the new configuration
// beside the old one (Move), leaves cell A and tries cell B with the new secret. B
// accepts (a Link with Next): the new configuration becomes the current one
// (MoveCommit) and the queue goes to B. B still unreachable at the deadline: back to
// A (MoveBack), the queue goes to A. The move is in the snapshot without its secret,
// so that a restart in the middle goes on until the deadline counted from the order.
//
// While frozen the cloud cannot confirm an alert: one for its channels fails at once
// and the hub turns to the GSM, as when the internet is down.

// moving is a move under way. Its secret is never here: only in the new configuration
// the application wrote (0600).
type moving struct {
	Ref    string    `json:"ref"`     // the agent.move
	URLs   []string  `json:"urls"`    // cell B's broker: its hosts take the photos once there
	GiveUp time.Time `json:"give_up"` // B not reached by then: back to A
}

// clone copies a move to save, nil staying nil.
func (m *moving) clone() *moving {
	if m == nil {
		return nil
	}
	c := *m
	c.URLs = slices.Clone(m.URLs)
	return &c
}

// Move: write the new configuration beside the current one (atomic, the secret
// 0600), leave cell A and connect to cell B with it until told otherwise. A failure
// to write it is told back (MoveFailed).
type Move struct {
	Ref    string
	URLs   []string
	Secret string // never logged
	GiveUp time.Time
}

// String hides the secret.
func (m Move) String() string { return "agent.Move{" + m.Ref + " secret:***}" }

// GoString hides the secret for %#v.
func (m Move) GoString() string { return m.String() }

// MoveCommit: cell B accepted the agent; the new configuration becomes the current
// one, the old one kept once.
type MoveCommit struct{ Ref string }

// MoveBack: forget the new configuration and connect to cell A again.
type MoveBack struct {
	Ref    string
	Reason string // contract.MoveUnreachable or contract.MoveNoSave
}

// Moved: a move ended; publish agent.moved.
type Moved struct{ Report contract.MovedData }

func (Move) isAction()       {}
func (MoveCommit) isAction() {}
func (MoveBack) isAction()   {}
func (Moved) isAction()      {}

// MoveFailed says the application could not write the new configuration: the agent
// stays on cell A.
type MoveFailed struct {
	Now time.Time
	Ref string
}

func (i MoveFailed) at() time.Time { return i.Now }

// Moving says whether a move is under way: nothing goes up meanwhile.
func (r *Relay) Moving() bool { return r.s.Move != nil }

// MoveRef is the agent.move under way, "" if none.
func (r *Relay) MoveRef() string {
	if r.s.Move == nil {
		return ""
	}
	return r.s.Move.Ref
}

// checkMove applies the rules of agent.move: valid, not expired (against the clock as
// it is), and no other move under way.
func (r *Relay) checkMove(now time.Time, env contract.Envelope) string {
	d, why := valid[contract.MoveData](env)
	if why = orExpired(why, now, &d.Until); why != "" {
		return why
	}
	if r.s.Move != nil {
		return "a move is already under way"
	}
	return ""
}

// startMove freezes what goes up and asks the application to move. The alerts the
// cloud has not confirmed fail now: it cannot confirm them while the agent moves.
func (r *Relay) startMove(now time.Time, env contract.Envelope) []Action {
	d, _ := contract.DecodeData[contract.MoveData](env) // checked above
	r.s.Move = &moving{Ref: env.ID, URLs: d.URLs, GiveUp: now.Add(d.GiveUp())}
	clear(r.owed)
	out := r.failUnconfirmed(func(pendingAlert) bool { return true })
	out = append(out, Move{Ref: env.ID, URLs: d.URLs, Secret: d.Secret, GiveUp: r.s.Move.GiveUp})
	return append(out, StatusChanged{Status: r.Status(now)})
}

// arrived ends a move on cell B: its hosts take the photos from now on.
func (r *Relay) arrived() []Action {
	m := r.s.Move
	r.s.Move = nil
	r.cfg.CloudURLs = m.URLs
	return []Action{MoveCommit{Ref: m.Ref}, Moved{Report: contract.MovedData{Ref: m.Ref, To: contract.MovedToB}}}
}

// back ends a move on cell A.
func (r *Relay) back(now time.Time, reason string) []Action {
	m := r.s.Move
	r.s.Move = nil
	return []Action{
		MoveBack{Ref: m.Ref, Reason: reason},
		Moved{Report: contract.MovedData{Ref: m.Ref, To: contract.MovedBack, Reason: reason}},
		StatusChanged{Status: r.Status(now)},
	}
}

// moveFailed takes the application's word that the new configuration was not written.
func (r *Relay) moveFailed(f MoveFailed) []Action {
	if r.s.Move == nil || r.s.Move.Ref != f.Ref {
		return nil
	}
	return r.back(f.Now, contract.MoveNoSave)
}

// expireMove goes back to A once the deadline passed.
func (r *Relay) expireMove(now time.Time) []Action {
	if r.s.Move == nil || now.Before(r.s.Move.GiveUp) {
		return nil
	}
	return r.back(now, contract.MoveUnreachable)
}
