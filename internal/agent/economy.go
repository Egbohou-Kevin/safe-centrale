package agent

import (
	"encoding/json"
	"time"

	"safehome/internal/contract"
)

// EconomyEvery is how often the heartbeat and agent.status reach the cloud on mobile
// data paid by the client (docs/agent.md), instead of every minute.
const EconomyEvery = 5 * time.Minute

// sentMark is what the cloud last got of a repeated message (heartbeat, agent.status).
type sentMark struct {
	at   time.Time
	mark string // for agent.status, the link and transport: a change goes at once
}

// repeated tells whether a message is one of those repeated every minute, which
// economy thins.
func repeated(typ string) bool {
	return typ == contract.TypeHeartbeat || typ == contract.TypeAgentStatus
}

// markOf is what must reach the cloud at once when it changes: the link said by
// agent.status (it overwrites the "down" the cloud's broker published when the link
// broke). Empty for the heartbeat.
func markOf(typ string, data json.RawMessage) string {
	if typ != contract.TypeAgentStatus {
		return ""
	}
	var d contract.AgentStatusData
	if json.Unmarshal(data, &d) != nil {
		return ""
	}
	return d.Uplink + " " + d.Transport
}

// thinned tells whether economy drops this repeated message: the cloud got one less
// than EconomyEvery ago, nothing of it is waiting (a waiting one is replaced, so the
// cloud gets the latest), and it says nothing new.
func (r *Relay) thinned(now time.Time, env contract.Envelope, key string) bool {
	if r.cfg.Economy <= 0 || !repeated(env.Type) {
		return false
	}
	for _, q := range r.queue {
		if q.Key == key {
			return false
		}
	}
	last, ok := r.got[key]
	return ok && now.Sub(last.at) < r.cfg.Economy && markOf(env.Type, env.Data) == last.mark
}

// noteSent remembers when the cloud got a repeated message. In memory only: after a
// restart, the first one goes at once.
func (r *Relay) noteSent(now time.Time, it Item) {
	if r.cfg.Economy <= 0 || it.Key == "" {
		return
	}
	var env contract.Envelope
	if json.Unmarshal(it.Payload, &env) != nil || !repeated(env.Type) {
		return
	}
	r.got[it.Key] = sentMark{at: now, mark: markOf(env.Type, env.Data)}
}
