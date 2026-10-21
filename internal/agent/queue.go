package agent

import (
	"cmp"
	"slices"
	"strings"

	"safehome/internal/contract"
)

// The queue for the cloud is not in the snapshot. It may hold 20000 items and 10 MB:
// saved with the rest of the state, it would be written again in full on the SD card
// for every item sent once the internet returns. The relay rather says what moves,
// one item at a time (QueueSave, QueueDrop), the application keeps each item on its
// own, and RestoreQueue puts back at start what it read.
//
// Within one task, the application carries out the actions before it saves the
// snapshot: a crash in between leaves the store ahead of the snapshot, which
// RestoreQueue reconciles.

// QueueSave: keep this item of the queue on disk, new or changed (numbered). Done
// before the item is sent, so that it keeps its number after a crash.
type QueueSave struct{ Item Item }

// QueueDrop: these items left the queue (sent, replaced, dropped); their copy on
// disk goes.
type QueueDrop struct{ IDs []string }

func (QueueSave) action() {}
func (QueueDrop) action() {}

// push adds an item at the end of the queue, and returns the action that saves it.
func (r *Relay) push(it Item) Action {
	r.order++
	it.Order = r.order
	r.queue = append(r.queue, it)
	return QueueSave{Item: it}
}

// removeIf takes the items that match out of the queue. It returns them, with the
// action that drops them from the disk.
func (r *Relay) removeIf(match func(Item) bool) ([]Item, []Action) {
	var gone []Item
	r.queue = slices.DeleteFunc(r.queue, func(it Item) bool {
		if match(it) {
			gone = append(gone, it)
			return true
		}
		return false
	})
	return gone, dropOf(gone)
}

// dropOf returns the action that drops these items from the disk, none if there is
// none.
func dropOf(items []Item) []Action {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return []Action{QueueDrop{IDs: ids}}
}

// Next returns the item to send now: alarm and alert messages first, then device
// events, then states, each in order of arrival, then what the cloud asked for again.
// An item gets its number for the cloud here, at its first attempt, and keeps it:
// the QueueSave returned then must be carried out before the item is sent.
func (r *Relay) Next() (Item, []Action, bool) {
	best := -1
	for i, it := range r.queue {
		if best < 0 || it.Prio > r.queue[best].Prio {
			best = i
			if it.Prio == PriorityAlarm {
				break
			}
		}
	}
	if best >= 0 {
		it := &r.queue[best]
		if it.Seq != 0 {
			return *it, nil, true
		}
		r.number(it)
		return *it, []Action{QueueSave{Item: *it}}, true
	}
	if len(r.replay) > 0 {
		return r.replay[0], nil, true
	}
	return Item{}, nil, false
}

// RestoreQueue puts back the items read from the queue's store, in any order, after
// Restore. It returns what the store must do to agree with the snapshot:
//   - an item that only the snapshot holds (saved by a version that kept the queue
//     in it) is saved;
//   - of two items with the same key, the older goes: the crash came after the newer
//     was saved and before the older was dropped;
//   - an alert for a cloud channel that the snapshot does not follow goes: its
//     follow-up was lost in a crash, or the cloud already answered. The hub's
//     escalation does not wait for it: it turns to the GSM after 10 s on its own.
//
// The numbering goes on after the highest number found.
func (r *Relay) RestoreQueue(stored []Item) []Action {
	onDisk := make(map[string]bool, len(stored))
	for _, it := range stored {
		onDisk[it.ID] = true
	}
	var out []Action
	for _, it := range r.queue {
		if !onDisk[it.ID] {
			stored = append(stored, it)
			out = append(out, QueueSave{Item: it})
		}
	}
	slices.SortFunc(stored, func(a, b Item) int {
		return cmp.Or(cmp.Compare(a.Order, b.Order), strings.Compare(a.ID, b.ID))
	})
	newest := map[string]string{} // key -> id of the newest item
	for _, it := range stored {
		r.s.Uplink = max(r.s.Uplink, it.Seq) // a number sent before the crash is never given again
		if it.Key != "" {
			newest[it.Key] = it.ID
		}
	}
	r.queue, r.order = nil, 0
	var gone []Item
	for _, it := range stored {
		if (it.Key != "" && newest[it.Key] != it.ID) || r.orphanAlert(it) {
			gone = append(gone, it)
			continue
		}
		r.queue = append(r.queue, it)
		r.order = max(r.order, it.Order)
	}
	return append(out, dropOf(gone)...)
}

// orphanAlert tells whether an item is an alert for a cloud channel that the relay
// does not follow.
func (r *Relay) orphanAlert(it Item) bool {
	if _, ok := r.s.Pending[it.ID]; ok || it.Topic != contract.AlertRequestTopic(r.cfg.Site) {
		return false
	}
	env, err := contract.ParseEnvelope(it.Payload)
	if err != nil {
		return false
	}
	d, err := contract.DecodeData[contract.AlertRequestData](env)
	return err == nil && cloudChannel(d.Channel)
}
