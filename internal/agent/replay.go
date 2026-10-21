package agent

import (
	"encoding/json"
	"slices"
	"time"

	"safehome/internal/contract"
)

// Numbering and replay (see docs/agent.md). The programs of the site number their own
// messages, and many never leave it: the agent numbers again what goes to the cloud,
// at the first attempt to send it, so that seq is continuous there and a hole means a
// message the cloud missed. What the cloud's broker acknowledged is kept in memory for
// a while (the journal), and sent again when the cloud asks (agent.replay).
//
// The journal is not saved: writing it to the SD card at every message would wear it
// out. After a restart of the agent, what was sent before is no longer held, and the
// answer says so.

// number gives an item the next number, at its first attempt to leave.
func (r *Relay) number(it *Item) {
	r.s.Uplink++
	stamp(it, r.s.Uplink)
}

// stamp writes seq into the item and into its envelope, which keeps the order of
// its fields. The payload was checked on its way in; should it not be read again, it
// goes as it is.
func stamp(it *Item, seq uint64) {
	it.Seq = seq
	var env contract.Envelope
	if json.Unmarshal(it.Payload, &env) != nil {
		return
	}
	env.Seq = seq
	if b, err := json.Marshal(env); err == nil {
		it.Payload = b
	}
}

// sent takes an acknowledged item out of the queue, or of the replays, and keeps a
// numbered one in the journal.
func (r *Relay) sent(now time.Time, id string) []Action {
	if i := slices.IndexFunc(r.queue, func(it Item) bool { return it.ID == id }); i >= 0 {
		it := r.queue[i]
		r.queue = slices.Delete(r.queue, i, i+1)
		r.noteSent(now, it)
		if it.Seq != 0 {
			r.remember(it)
		}
		return dropOf([]Item{it})
	}
	if i := slices.IndexFunc(r.replay, func(it Item) bool { return it.ID == id }); i >= 0 {
		r.replay = slices.Delete(r.replay, i, i+1)
	}
	return nil
}

// remember keeps a sent item in the journal, within its bounds: the oldest go first.
func (r *Relay) remember(it Item) {
	r.journal = append(r.journal, it)
	r.journalBytes += len(it.Payload)
	for len(r.journal) > r.cfg.JournalItems || (r.journalBytes > r.cfg.JournalBytes && len(r.journal) > 1) {
		r.journalBytes -= len(r.journal[0].Payload)
		r.journal = slices.Delete(r.journal, 0, 1)
	}
}

// replayRange queues again what the journal holds between d.From and d.To. Numbers
// still in the queue are not missing: they are on their way. The replays leave after
// everything else, and never retained: an old state must not replace the current one
// on the cloud's broker.
func (r *Relay) replayRange(d contract.ReplayData) []Action {
	held := map[uint64]Item{}
	for _, it := range r.journal {
		if it.Seq >= d.From && it.Seq <= d.To {
			held[it.Seq] = it
		}
	}
	queued := map[uint64]bool{}
	for _, it := range r.queue {
		if it.Seq != 0 {
			queued[it.Seq] = true
		}
	}
	report := contract.ReplayedData{From: d.From, To: d.To, Last: r.s.Uplink}
	for seq := d.From; seq <= d.To; seq++ {
		it, ok := held[seq]
		switch {
		case queued[seq]:
			report.Resent++
		case !ok:
			if n := len(report.Missing); n > 0 && report.Missing[n-1].Last == seq-1 {
				report.Missing[n-1].Last = seq
			} else {
				report.Missing = append(report.Missing, contract.SeqRange{First: seq, Last: seq})
			}
		default:
			report.Resent++
			if !slices.ContainsFunc(r.replay, func(q Item) bool { return q.ID == it.ID }) {
				it.Retain, it.Key = false, ""
				r.replay = append(r.replay, it)
			}
		}
	}
	return []Action{Replayed{Report: report}}
}
