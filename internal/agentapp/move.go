package agentapp

import (
	"time"

	"safehome/internal/agent"
	"safehome/internal/contract"
)

// Mover carries out a move to another cell of the cloud (agent.move, see
// internal/agent/move.go): the new configuration on disk (internal/agentconfig) and
// the connection.
type Mover interface {
	// Pending says whether this run connects with the new configuration of a move
	// under way (cell B's), and the agent.move that wrote it.
	Pending() (ref string, ok bool)
	// Current is the agent.move that wrote the current configuration, "" if none.
	Current() string
	Write(ref string, urls []string, secret string) error // the new configuration, beside the current one
	Commit() error                                        // cell B accepted: it becomes the current one
	Discard() error                                       // back to A: forgotten
	// Reconnect connects to the cloud again with what the disk holds now: the
	// program restarts, once its loops have saved their state.
	Reconnect()
}

// commitTries bounds the attempts to make cell B's configuration the current one.
const commitTries = 3

// moveLeft is what a stop left of a move between the disk and the core.
type moveLeft int

const (
	settled     moveLeft = iota // the disk and the core agree
	uncommitted                 // B accepted the agent, its configuration is still pending: commit it
	stray                       // a new configuration the core does not know of: forget it
	landed                      // B's configuration is the current one, the core did not save it
	lost                        // a move the core knows of without its configuration
)

// leftOf compares the disk with the core. It is read in New, before the core's
// loop runs: the core belongs to the loop from then on.
func leftOf(core *agent.Relay, m Mover) moveLeft {
	var next, current string
	var pending bool
	if m != nil {
		next, pending = m.Pending()
		current = m.Current()
	}
	switch moving := core.Moving(); {
	case pending && !moving && next != "" && next == core.Arrived():
		return uncommitted
	case pending && !moving:
		return stray
	case !pending && moving && current == core.MoveRef():
		return landed
	case !pending && moving:
		return lost
	}
	return settled
}

// readMove reads what a stop left of a move, in New: before any goroutine runs the
// core. A configuration B accepted already is no move's connection: the link it
// brings up is the agent's usual one.
func (a *App) readMove() {
	a.left = leftOf(a.core, a.deps.Mover)
	if a.deps.Mover != nil {
		_, pending := a.deps.Mover.Pending()
		a.nextCloud.Store(pending && a.left != uncommitted)
	}
}

// dispatchMove carries out what the core decided about a move. It runs on the loop's
// goroutine; the secret is never logged.
func (a *App) dispatchMove(now time.Time, act agent.Action) {
	switch v := act.(type) {
	case agent.Move:
		err := errNoMover
		if a.deps.Mover != nil {
			err = a.deps.Mover.Write(v.Ref, v.URLs, v.Secret)
		}
		if err != nil {
			a.log.Error("the new configuration of the move could not be written: staying on this cloud", "ref", v.Ref, "err", err)
			a.later(func(now time.Time) agent.Input { return agent.MoveFailed{Now: now, Ref: v.Ref} })
			return
		}
		a.log.Info("moving to another cell of the cloud: nothing goes up until it ends", "ref", v.Ref, "urls", v.URLs, "give_up", v.GiveUp)
		a.deps.Mover.Reconnect()
	case agent.MoveCommit:
		a.nextCloud.Store(false)
		a.commit(v.Ref)
	case agent.MoveBack:
		a.log.Warn("the move did not end on the new cell: back to the old one", "ref", v.Ref, "reason", v.Reason)
		if a.deps.Mover == nil {
			return
		}
		if err := a.deps.Mover.Discard(); err != nil {
			a.log.Error("the new configuration of the move could not be forgotten", "ref", v.Ref, "err", err)
		}
		if a.nextCloud.Load() {
			a.deps.Mover.Reconnect()
		}
	case agent.Moved:
		a.publishOwn(now, contract.AgentMovedTopic(a.cfg.Site), contract.TypeAgentMoved, v.Report, false)
	}
}

// commit makes cell B's configuration the current one, and says whether it did. A
// failure leaves it pending on disk: the core remembers the move arrived, so the next
// start commits it again rather than go back to A, whose secret may be gone.
func (a *App) commit(ref string) bool {
	var err error
	for range commitTries {
		if err = a.deps.Mover.Commit(); err == nil {
			a.log.Info("moved: the new cell is the current one", "ref", ref)
			return true
		}
	}
	a.log.Error("the new cell accepted the agent, but its configuration could not become the current one: tried again at the next start", "ref", ref, "err", err)
	return false
}

// settleMove makes the disk and the core agree after a stop between the two (see
// leftOf). Run calls it before the core's loop starts.
func (a *App) settleMove() {
	switch a.left {
	case uncommitted:
		// Connected with B's configuration, whose cloud the core does not know yet:
		// once current, the agent starts again on it.
		if a.commit(a.core.Arrived()) {
			a.deps.Mover.Reconnect()
		}
	case stray:
		a.log.Warn("a new configuration without its move: forgotten, back to the old cell")
		a.dispatchMove(time.Time{}, agent.MoveBack{Reason: "no move under way"})
	case landed:
		ref := a.core.MoveRef()
		a.log.Info("the new cell's configuration became the current one before the stop: the move ended there", "ref", ref)
		a.dispatch(a.life, a.deps.Clock.Now(), a.core.Landed(ref))
	case lost:
		ref := a.core.MoveRef()
		a.later(func(now time.Time) agent.Input { return agent.MoveFailed{Now: now, Ref: ref} })
	}
}

// later hands an input to the core from outside its loop's goroutine.
func (a *App) later(build func(now time.Time) agent.Input) {
	go a.submit(build)
}
