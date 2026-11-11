package agent

import (
	"slices"
	"time"
)

// What reached the cloud is remembered for a while, by its id: the hub's broker may
// give a message again after the agent sent it and it left the queue. Mosquitto
// saves its sessions every few minutes (autosave_interval 300 s in deploy/mosquitto):
// after a power cut, it brings back what it held for the agent at its last save,
// some of which the agent took and sent since; and acknowledgements to the hub's
// broker go out in batches, lost if the connection resets. Queued again, such a
// message would take a new number, which the cloud would drop as a duplicate of its
// id: a hole that never closes. A photo announced again would be followed again, an
// alert request would wait for a confirmation that cannot come.
//
// The ids are kept on disk, as tombstones in the queue's store: the item's document
// is replaced by a few bytes when it is sent, instead of removed, and goes once it is
// older than sentMemory. Retained states are known otherwise (see newState).

// sentMemory is how long the id of a message that reached the cloud is kept: longer
// than the hub's broker lets pass between two saves.
const sentMemory = 10 * time.Minute

// tomb is the id of a message that reached the cloud, and when.
type tomb struct {
	id string
	at time.Time
}

// bury remembers an item that reached the cloud, and returns the action that leaves
// its tombstone on disk in place of the item.
func (r *Relay) bury(now time.Time, it Item) []Action {
	r.tombs = append(r.tombs, tomb{id: it.ID, at: now})
	r.buried[it.ID] = true
	stone := Item{ID: it.ID, Topic: it.Topic, At: now, Seq: it.Seq, Order: it.Order, Sent: true}
	return append([]Action{QueueSave{Item: stone}}, r.forgetSent(now)...)
}

// forgetSent drops the tombstones older than sentMemory, and the oldest beyond as many
// as the queue may hold items.
func (r *Relay) forgetSent(now time.Time) []Action {
	var ids []string
	for len(r.tombs) > 0 && (!now.Before(r.tombs[0].at.Add(sentMemory)) || len(r.tombs) > r.cfg.QueueItems) {
		ids = append(ids, r.tombs[0].id)
		delete(r.buried, r.tombs[0].id)
		r.tombs = r.tombs[1:]
	}
	if len(ids) == 0 {
		return nil
	}
	return []Action{QueueDrop{IDs: ids}}
}

// restoreSent puts back the tombstones read from the queue's store, oldest first, and
// returns the items that are not tombstones.
func (r *Relay) restoreSent(stored []Item) []Item {
	r.tombs, r.buried = nil, map[string]bool{}
	items := slices.DeleteFunc(slices.Clone(stored), func(it Item) bool { return it.Sent })
	var stones []Item
	for _, it := range stored {
		if it.Sent {
			stones = append(stones, it)
		}
	}
	slices.SortStableFunc(stones, func(a, b Item) int { return a.At.Compare(b.At) })
	for _, it := range stones {
		r.tombs = append(r.tombs, tomb{id: it.ID, at: it.At})
		r.buried[it.ID] = true
	}
	return items
}
