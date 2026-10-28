package outbox

import "safehome/internal/contract"

// priority orders what a full queue drops: the lowest goes first, the oldest of it
// first. The same three levels as the agent's queue for the cloud.
type priority int

const (
	priorityState  priority = iota // periodic states and the heartbeat: a newer one follows
	priorityNormal                 // device events, sessions, the other orders
	priorityAlarm                  // alarms, alerts, siren commands and their answers
)

// priorities are the topics that are not normal, keyed by the parts after the site;
// a device topic is keyed device/+/{kind}. alarm/* and alert/* are matched by their
// first part.
var priorities = map[string]priority{
	"device/+/cmd":     priorityAlarm, // the siren
	"device/+/ack":     priorityAlarm, // its answer: the hub knows whether it sounds
	"device/+/state":   priorityState,
	"system/heartbeat": priorityState,
	"system/health":    priorityState,
	"system/devices":   priorityState,
	"registry/devices": priorityState,
	"gsm/status":       priorityState,
	"zigbee/status":    priorityState,
	"video/status":     priorityState,
	"agent/status":     priorityState,
}

// priorityOf is the priority of a queued message: that of its topic, except that a
// clear of a retained message (retained, no data) is never noise. It removes what the
// broker would otherwise keep: a removed door's retained "open" would block every
// arming, so it goes no sooner than the events.
func priorityOf(it *Item) priority {
	p := topicPriority(it.Topic)
	if it.Retain && len(it.Data) == 0 && p < priorityNormal {
		return priorityNormal
	}
	return p
}

// topicPriority is the priority of a message on topic. A topic outside the contract is
// a request to another program (Zigbee2MQTT: a siren to sound, a device to pair): rare,
// and never dropped before the noise.
func topicPriority(topic string) priority {
	_, parts, ok := contract.ParseTopic(topic)
	if !ok {
		return priorityAlarm
	}
	if parts[0] == "alarm" || parts[0] == "alert" {
		return priorityAlarm
	}
	key := parts[0]
	switch {
	case len(parts) == 3 && parts[0] == "device":
		key = "device/+/" + parts[2]
	case len(parts) == 4 && parts[0] == "device":
		key = "device/+/" + parts[2] // device/{id}/state/{capability}
	case len(parts) == 2:
		key = parts[0] + "/" + parts[1]
	}
	if p, ok := priorities[key]; ok {
		return p
	}
	return priorityNormal
}
