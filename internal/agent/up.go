package agent

import (
	"encoding/json"
	"slices"
	"time"

	"safehome/internal/contract"
)

// From the hub to the cloud: what goes up, in which order, and what the queue drops
// when it is full.

// local takes a message of the hub's broker, or of the agent itself (own), and
// queues it for the cloud if it goes there.
func (r *Relay) local(now time.Time, topic string, payload []byte, own bool) []Action {
	route, env, ok := r.goesUp(topic, payload)
	if !ok || (route.own && !own) || slices.ContainsFunc(r.queue, func(it Item) bool { return it.ID == env.ID }) {
		return nil // not for the cloud, given back by the broker, or the same message again (QoS 1)
	}
	if route.retain && !own && !r.newState(topic, env.ID) {
		return nil
	}
	if inc := incidentOf(env); inc != "" {
		r.s.Incidents[inc] = now
	}
	if env.Type == contract.TypeAlertRequest && !r.followAlert(now, env) {
		// The hub would wait for a confirmation that cannot come: tell it now, its
		// escalation goes to the GSM and sends this again when the link returns.
		return []Action{FailAlert{Ref: env.ID}}
	}
	it := Item{ID: env.ID, Topic: topic, Payload: json.RawMessage(payload), Prio: route.prio, Retain: route.retain, At: now}
	var replaced []Action
	if route.retain || env.Type == contract.TypeHeartbeat {
		if r.thinned(now, env, topic) {
			return nil // economy: the cloud got one a moment ago
		}
		replaced = r.replaceKeyed(&it, topic)
	}
	// The newer is saved before the older is dropped: a crash in between leaves both
	// on disk, never neither.
	out := append([]Action{r.push(it)}, replaced...)
	out = append(out, r.bound()...)
	if env.Type == contract.TypePhotoTaken {
		out = append(out, r.photoTaken(now, env)...)
	}
	return out
}

// newState tells whether a retained message of the hub was not queued for the cloud
// yet, and remembers it. The hub's broker gives the retained messages again at each
// new connection of the agent: sent again, an old state would take a new number,
// which the cloud would drop as a duplicate of its id, and the hole it leaves would
// never close. After a restart of the agent, a state that changed meanwhile has a new
// id, and goes.
func (r *Relay) newState(topic, id string) bool {
	if r.s.Retained[topic] == id {
		return false
	}
	r.s.Retained[topic] = id
	return true
}

// goesUp reads a message of the hub and tells whether the contract sends it to the
// cloud, and by which route.
func (r *Relay) goesUp(topic string, payload []byte) (upRoute, contract.Envelope, bool) {
	site, parts, ok := contract.ParseTopic(topic)
	if !ok || site != r.cfg.Site {
		return upRoute{}, contract.Envelope{}, false
	}
	route, ok := upRoutes[routeKey(parts)]
	if !ok {
		return upRoute{}, contract.Envelope{}, false
	}
	env, err := contract.ParseEnvelope(payload)
	if err != nil || env.Site != r.cfg.Site || !slices.Contains(route.types, env.Type) {
		return upRoute{}, contract.Envelope{}, false // the components that read it log what is wrong with it
	}
	if route.device {
		d, err := contract.DecodeData[contract.DeviceEventData](env)
		if err != nil || !slices.Contains(upCapabilities, d.Capability) || d.Code != "" {
			return upRoute{}, contract.Envelope{}, false
		}
	}
	return route, env, true
}

// followAlert starts waiting for the cloud's confirmation of an alert for one of its
// channels. It returns false if the link is down: no confirmation can come.
func (r *Relay) followAlert(now time.Time, env contract.Envelope) bool {
	d, err := contract.DecodeData[contract.AlertRequestData](env)
	if err != nil || !cloudChannel(d.Channel) {
		return true // the GSM's business: it only goes up for the record
	}
	if !r.up {
		return false
	}
	r.s.Pending[env.ID] = pendingAlert{Deadline: now.Add(r.cfg.ConfirmWait), Forget: now.Add(alertMemory), Incident: d.Incident}
	return true
}

func cloudChannel(ch string) bool { return ch == contract.ChannelApp || ch == contract.ChannelWhatsApp }

// replaceKeyed makes it replace the waiting item with the same key, of which only
// the latest is worth sending, and returns the action that drops the older. An older
// one may have been tried without an acknowledgement: the newer takes its number, so
// that replacing it leaves no hole in the cloud.
func (r *Relay) replaceKeyed(it *Item, key string) []Action {
	it.Key = key
	older, drop := r.removeIf(func(q Item) bool { return q.Key == key })
	for _, q := range older {
		if q.Seq != 0 {
			stamp(it, q.Seq)
		}
	}
	return drop
}

// incidentOf reads the incident a message is about, if any.
func incidentOf(env contract.Envelope) string {
	var d struct {
		Incident string `json:"incident"`
	}
	if json.Unmarshal(env.Data, &d) != nil {
		return ""
	}
	return d.Incident
}

// bound keeps the queue within its limits: it drops the least important items
// first, the oldest of them first.
func (r *Relay) bound() []Action {
	size := 0
	for _, it := range r.queue {
		size += len(it.Payload)
	}
	var gone []Item
	var out []Action
	for len(r.queue) > r.cfg.QueueItems || (size > r.cfg.QueueBytes && len(r.queue) > 1) {
		i := r.victim()
		it := r.queue[i]
		r.queue = slices.Delete(r.queue, i, i+1)
		size -= len(it.Payload)
		gone = append(gone, it)
		if _, ok := r.s.Pending[it.ID]; ok {
			out = append(out, r.giveUp(it.ID)) // an alert that will not reach the cloud: the hub must know
		}
	}
	return append(out, r.dropped(gone, "queue full")...)
}

// dropped counts the items that left the queue without reaching the cloud, and
// returns the actions that say so. A retained state among them goes again if the
// hub's broker gives it back.
func (r *Relay) dropped(gone []Item, reason string) []Action {
	if len(gone) == 0 {
		return nil
	}
	r.s.Dropped += len(gone)
	for _, it := range gone {
		if r.s.Retained[it.Topic] == it.ID {
			delete(r.s.Retained, it.Topic)
		}
	}
	return append(dropOf(gone), Dropped{Count: len(gone), Reason: reason})
}

// victim is the index of the item to drop: the oldest of the lowest priority.
func (r *Relay) victim() int {
	best := 0
	for i, it := range r.queue {
		if it.Prio < r.queue[best].Prio {
			best = i
		}
		if r.queue[best].Prio == PriorityState {
			break
		}
	}
	return best
}
