package agentapp

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"safehome/internal/agent"
	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
)

// queued reads the size of the core's queue, on its loop.
func (r *rig) queued() int {
	n := -1
	r.app.loop.Do(context.Background(), func(now time.Time) []agent.Action {
		n = r.app.core.Status(now).Queued
		return nil
	})
	return n
}

// Draining the queue after an outage writes one small file per item and a tiny
// counter, never the whole queue nor the snapshot again: the SD card would not last.
func TestDrainingTheQueueWritesItemsNotTheQueue(t *testing.T) {
	const n = 1000
	d := newDisk()
	// The snapshot as heavy as it gets: the ids of the last messages from the cloud
	// fill it (some 59 KB).
	ids := contract.NewIDGenerator()
	seen := make([]string, agent.DefaultConfig(site).SeenSize)
	for i := range seen {
		seen[i] = ids.New()
	}
	snap, _ := json.Marshal(agent.Snapshot{Seen: seen})
	d.store.Save(keyAgent, snap)
	r := newRig(t, d)
	for range n {
		r.fromHub(topic("alarm", "event"), contract.TypeAlarmArmed, contract.ArmedData{})
	}
	drivertest.WaitFor(t, "the queue to fill", func() bool { return r.queued() > n })
	q, s := r.disk.queue, r.disk.store
	q.mu.Lock()
	puts, removed := len(q.written), q.removed
	q.mu.Unlock()
	saves, counts := s.Saves(keyAgent), s.Saves(keyUplink)

	r.app.LinkChanged(true, "mqtts")
	drivertest.WaitFor(t, "the queue to drain", func() bool { return len(r.cloud.ofType(contract.TypeAlarmArmed)) == n && r.queued() == 0 })
	q.mu.Lock()
	defer q.mu.Unlock()
	itemWrites, snapWrites, countWrites := q.written[puts:], s.Sizes(keyAgent)[saves:], s.Sizes(keyUplink)[counts:]
	t.Logf("draining %d items wrote %d bytes of items, %d bytes of snapshots (%d saves) and %d bytes of counter",
		n, sum(itemWrites), sum(snapWrites), len(snapWrites), sum(countWrites))
	// Each item is written once more, with its number, then removed; a few agent.status
	// join it.
	if len(itemWrites) < n || len(itemWrites) > n+10 || q.removed-removed < n || len(q.items) != 0 {
		t.Fatalf("%d items written and %d removed for %d sent, %d left", len(itemWrites), q.removed-removed, n, len(q.items))
	}
	if big := slices.Max(itemWrites); big > 2048 {
		t.Fatalf("an item of %d bytes", big)
	}
	// The counter is saved for each new number, and is tiny; the snapshot hardly moves.
	if len(countWrites) < n || len(countWrites) > n+10 || sum(countWrites) > 64*len(countWrites) {
		t.Fatalf("%d counters saved, %d bytes", len(countWrites), sum(countWrites))
	}
	if len(snapWrites) > 10 {
		t.Fatalf("%d snapshots saved for %d items sent", len(snapWrites), n)
	}
}

func sum(sizes []int) int {
	total := 0
	for _, n := range sizes {
		total += n
	}
	return total
}
