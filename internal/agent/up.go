package agent

import (
	"encoding/json"
	"slices"
	"time"

	"safehome/internal/contract"
)

// From the hub to the cloud: what goes up, in which order, and what the queue drops
// when it is full.

// local takes a message of the hub's broker and queues it for the cloud if it goes
// there.
func (r *Relay) local(now time.Time, topic string, payload []byte) []Action {
	route, env, ok := r.goesUp(topic, payload)
	if !ok || slices.ContainsFunc(r.s.Queue, func(it Item) bool { return it.ID == env.ID }) {
		return nil // not for the cloud, or the same message again (QoS 1)
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
	if route.retain || env.Type == contract.TypeHeartbeat {
		if r.thinned(now, env, topic) {
			return nil // economy: the cloud got one a moment ago
		}
		r.replaceKeyed(&it, topic)
	}
	r.s.Queue = append(r.s.Queue, it)
	out := r.bound()
	if env.Type == contract.TypePhotoTaken {
		out = append(out, r.photoTaken(now, env)...)
	}
	return out
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
// the latest is worth sending. An older one may have been tried without an
// acknowledgement: the newer takes its number, so that replacing it leaves no hole
// in the cloud.
func (r *Relay) replaceKeyed(it *Item, key string) {
	it.Key = key
	for _, q := range r.s.Queue {
		if q.Key == key && q.Seq != 0 {
			stamp(it, q.Seq)
		}
	}
	r.s.Queue = slices.DeleteFunc(r.s.Queue, func(q Item) bool { return q.Key == key })
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
	for _, it := range r.s.Queue {
		size += len(it.Payload)
	}
	dropped := 0
	var out []Action
	for len(r.s.Queue) > r.cfg.QueueItems || (size > r.cfg.QueueBytes && len(r.s.Queue) > 1) {
		victim := r.victim()
		id := r.s.Queue[victim].ID
		size -= len(r.s.Queue[victim].Payload)
		r.s.Queue = slices.Delete(r.s.Queue, victim, victim+1)
		dropped++
		if _, ok := r.s.Pending[id]; ok {
			delete(r.s.Pending, id) // an alert that will not reach the cloud: the hub must know
			out = append(out, FailAlert{Ref: id})
		}
	}
	if dropped == 0 {
		return out
	}
	r.s.Dropped += dropped
	return append(out, Dropped{Count: dropped, Reason: "queue full"})
}

// victim is the index of the item to drop: the oldest of the lowest priority.
func (r *Relay) victim() int {
	for _, p := range []Priority{PriorityState, PriorityNormal, PriorityAlarm} {
		for i, it := range r.s.Queue {
			if it.Prio == p {
				return i
			}
		}
	}
	return 0
}
