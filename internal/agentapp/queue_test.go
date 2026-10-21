package agentapp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"safehome/internal/agent"
	"safehome/internal/contract"
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

// Draining the queue after an outage writes one small file per item and a small
// snapshot, never the whole queue again: the SD card would not last.
func TestDrainingTheQueueWritesItemsNotTheQueue(t *testing.T) {
	const n = 1000
	r := newRig(t, nil)
	for range n {
		r.fromHub(topic("alarm", "event"), contract.TypeAlarmArmed, contract.ArmedData{})
	}
	eventually(t, "the queue to fill", func() bool { return r.queued() > n })
	q, s := r.disk.queue, r.disk.store
	q.mu.Lock()
	s.mu.Lock()
	puts, saves, removed := len(q.written), len(s.written), q.removed
	s.mu.Unlock()
	q.mu.Unlock()

	r.app.LinkChanged(true, "mqtts")
	eventually(t, "the queue to drain", func() bool { return len(r.cloud.ofType(contract.TypeAlarmArmed)) == n && r.queued() == 0 })
	q.mu.Lock()
	defer q.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	itemWrites, snapWrites := q.written[puts:], s.written[saves:]
	// Each item is written once more, with its number, then removed; a few agent.status
	// join it.
	if len(itemWrites) < n || len(itemWrites) > n+10 || q.removed-removed < n || len(q.items) != 0 {
		t.Fatalf("%d items written and %d removed for %d sent, %d left", len(itemWrites), q.removed-removed, n, len(q.items))
	}
	if big := slices.Max(itemWrites); big > 2048 {
		t.Fatalf("an item of %d bytes", big)
	}
	// The snapshot is saved for each new number, and stays small.
	if len(snapWrites) > n+10 || slices.Max(snapWrites) > 1024 {
		t.Fatalf("%d snapshots saved, the largest of %d bytes", len(snapWrites), slices.Max(snapWrites))
	}
	t.Logf("draining %d items wrote %d bytes of items and %d bytes of snapshots", n, sum(itemWrites), sum(snapWrites))
}

func sum(sizes []int) int {
	total := 0
	for _, n := range sizes {
		total += n
	}
	return total
}

// A state saved by the previous version, with the queue in the snapshot, loads
// without losing an item: they move to the queue's store at start.
func TestAQueueSavedInTheSnapshotIsKeptAfterTheUpgrade(t *testing.T) {
	d := newDisk()
	ids := contract.NewIDGenerator()
	var items []string
	var sent []string
	for _, m := range []struct {
		topic, typ string
		prio       agent.Priority
		data       any
	}{
		{topic("system", "health"), contract.TypeHealth, agent.PriorityState, contract.HealthData{Links: map[string]string{}}},
		{topic("alarm", "event"), contract.TypeAlarmTriggered, agent.PriorityAlarm, triggered},
	} {
		env, err := contract.NewEnvelope(site, "hub", 1, ids.New(), t0, m.typ, m.data)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(env)
		items = append(items, fmt.Sprintf(`{"id":%q,"topic":%q,"payload":%s,"prio":%d,"retain":false,"at":"2026-10-20T21:00:00Z"}`,
			env.ID, m.topic, b, m.prio))
		sent = append(sent, env.ID)
	}
	d.store.docs[keyAgent] = []byte(`{"queue":[` + strings.Join(items, ",") + `],"pending":{},"incidents":{},"seen":[],` +
		`"dropped":0,"tokens":20,"tokens_at":"0001-01-01T00:00:00Z","uplink":7}`)

	r := newRig(t, d)
	if items, _ := d.queue.Load(); items[sent[0]] == nil || items[sent[1]] == nil {
		t.Fatalf("not moved to the queue's store: %v", slices.Collect(maps.Keys(items)))
	}
	r.app.LinkChanged(true, "mqtts")
	eventually(t, "the saved messages on the cloud", func() bool {
		return len(r.cloud.ofType(contract.TypeAlarmTriggered)) == 1 && len(r.cloud.ofType(contract.TypeHealth)) == 1
	})
	alarm, health := r.cloud.ofType(contract.TypeAlarmTriggered)[0], r.cloud.ofType(contract.TypeHealth)[0]
	if alarm.env.ID != sent[1] || health.env.ID != sent[0] || alarm.env.Seq != 8 || health.env.Seq <= alarm.env.Seq {
		t.Fatalf("alarm %s/%d, health %s/%d", alarm.env.ID, alarm.env.Seq, health.env.ID, health.env.Seq)
	}
	eventually(t, "a snapshot without the queue", func() bool {
		doc, _, _ := d.store.Load(keyAgent)
		return !strings.Contains(string(doc), `"queue"`)
	})
}
