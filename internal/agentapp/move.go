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
	// under way (cell B's).
	Pending() bool
	Write(urls []string, secret string) error // the new configuration, beside the current one
	Commit() error                            // cell B accepted: it becomes the current one
	Discard() error                           // back to A: forgotten
	// Reconnect connects to the cloud again with what the disk holds now: the
	// program restarts, once its loops have saved their state.
	Reconnect()
}

// dispatchMove carries out what the core decided about a move. It runs on the loop's
// goroutine; the secret is never logged.
func (a *App) dispatchMove(now time.Time, act agent.Action) {
	switch v := act.(type) {
	case agent.Move:
		err := errNoMover
		if a.deps.Mover != nil {
			err = a.deps.Mover.Write(v.URLs, v.Secret)
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
		if err := a.deps.Mover.Commit(); err != nil {
			a.log.Error("the new cell accepted the agent, but its configuration could not become the current one", "ref", v.Ref, "err", err)
			return
		}
		a.log.Info("moved: the new cell is the current one", "ref", v.Ref)
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

// settleMove makes the disk and the core agree after a stop between the two: a new
// configuration the core does not know of (the stop came before its state was
// saved) is forgotten, and the agent connects to the old cell again; a move the core
// knows of without its configuration goes back at once.
func (a *App) settleMove() {
	switch moving := a.core.Moving(); {
	case a.nextCloud.Load() && !moving:
		a.log.Warn("a new configuration without its move: forgotten, back to the old cell")
		a.dispatchMove(time.Time{}, agent.MoveBack{Reason: "no move under way"})
	case !a.nextCloud.Load() && moving:
		ref := a.core.MoveRef()
		a.later(func(now time.Time) agent.Input { return agent.MoveFailed{Now: now, Ref: ref} })
	}
}

// later hands an input to the core from outside its loop's goroutine.
func (a *App) later(build func(now time.Time) agent.Input) {
	go a.submit(build)
}
