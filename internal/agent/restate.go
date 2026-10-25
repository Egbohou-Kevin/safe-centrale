package agent

import (
	"maps"
	"slices"
	"time"

	"safehome/internal/contract"
)

// The current state at each connection (see docs/agent.md). The hub publishes its
// alarm state, its health and its device list when they change, and a state already
// sent never goes again (see newState): a cloud that lost it (its database reset, or
// it did not keep states yet) would show nothing until the next change. So at each new connection to
// the cloud, the relay says the hub's latest states again, as new messages: the
// application gives each a new id (the cloud drops a known id as a duplicate) and
// hands it back (Own). It then goes like any other message: queued, numbered,
// acknowledged, kept for a replay. The cloud keeps the newer state by its number, so
// the same state said again changes nothing there.
//
// The latest states are kept in memory only: the hub's broker gives them back at each
// connection of the agent, after a restart too.
//
// A state is said again at most once every restateFloor: a flapping link, or two
// agents on one client id (a cloned SD card) kicking each other off, would otherwise
// add messages, queue files and paid bytes at every connection. One said again a
// moment ago reached the cloud, or still waits in the queue.

// restateFloor is the least time between two restates of one topic.
const restateFloor = time.Minute

// Restate: say this state of the hub to the cloud again, as a new message: give it a
// new id, and hand it back to the relay (Own).
type Restate struct {
	Topic   string
	Payload []byte
}

func (Restate) isAction() {}

// restate is what a new connection to the cloud says again: the hub's latest states,
// except one waiting in the queue (it goes as it is). A state not known yet (the agent
// just started, the hub's broker has not given it back) is owed: it is said again
// when it comes.
func (r *Relay) restate(now time.Time) []Action {
	clear(r.owed)
	var out []Action
	for _, key := range slices.Sorted(maps.Keys(upRoutes)) {
		if !upRoutes[key].restate {
			continue
		}
		topic := contract.Topic(r.cfg.Site, key)
		switch payload, known := r.states[topic]; {
		case slices.ContainsFunc(r.queue, func(it Item) bool { return it.Key == topic }):
		case now.Before(r.restated[topic].Add(restateFloor)):
		case known:
			out = append(out, r.sayAgain(now, topic, payload))
		default:
			r.owed[topic] = true
		}
	}
	return out
}

// sayAgain says a state again, and remembers when.
func (r *Relay) sayAgain(now time.Time, topic string, payload []byte) Restate {
	r.restated[topic] = now
	return Restate{Topic: topic, Payload: payload}
}

// heard remembers the hub's latest state on a topic said again at each connection. If
// this connection still owes it to the cloud, it is owed no more: a new state goes as
// it is, a state given back by the hub's broker is said again.
func (r *Relay) heard(now time.Time, route upRoute, topic string, payload []byte, fresh bool) []Action {
	if !route.restate {
		return nil
	}
	r.states[topic] = payload
	if !r.owed[topic] {
		return nil
	}
	delete(r.owed, topic)
	if fresh {
		return nil
	}
	return []Action{r.sayAgain(now, topic, payload)}
}
