package agent

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	"safehome/internal/contract"
)

func TestDrainingTheQueueSavesOneItemAtATime(t *testing.T) {
	h := newHarness(t)
	for range 50 {
		h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	}
	before, _ := json.Marshal(h.r.Snapshot())
	if strings.Contains(string(before), `"queue"`) {
		t.Fatalf("the queue is in the snapshot: %s", before)
	}
	h.linkUp()
	for {
		it, acts, ok := h.r.Next()
		if !ok {
			break
		}
		s, u := of[QueueSave](acts), of[UplinkSave](acts)
		if len(acts) != 2 || len(s) != 1 || s[0].Item.ID != it.ID || s[0].Item.Seq != it.Seq || len(u) != 1 || u[0].Last != it.Seq {
			t.Fatalf("numbering %s: %+v", it.ID, acts)
		}
		h.keep(acts)
		acts = h.handle(Sent{Now: h.now, ID: it.ID})
		if d := of[QueueDrop](acts); len(d) != 1 || !reflect.DeepEqual(d[0].IDs, []string{it.ID}) {
			t.Fatalf("sent %s: %+v", it.ID, acts)
		}
	}
	if len(h.disk) != 0 {
		t.Fatalf("%d items left on disk", len(h.disk))
	}
	// The numbers went to their own document: the snapshot did not move.
	if after, _ := json.Marshal(h.r.Snapshot()); string(after) != string(before) || h.uplink != 50 {
		t.Fatalf("the snapshot went from %s to %s, the counter is %d", before, after, h.uplink)
	}
}

func TestARestartKeepsOrderPrioritiesNumbersAndKeys(t *testing.T) {
	h := newHarness(t)
	h.local(topic("system", "health"), contract.TypeHealth, health)
	first, _ := h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	h.local(topic("device", "dev_smoke", "event"), contract.TypeDeviceEvent, event("dev_smoke", contract.CapSmoke, contract.ValAlarm, ""))
	newer, _ := h.local(topic("system", "health"), contract.TypeHealth, contract.HealthData{Links: map[string]string{"zigbee": "down"}})
	second, _ := h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	h.linkUp()
	tried, _ := h.next() // numbered, not acknowledged

	h.restart()
	h.local(topic("system", "health"), contract.TypeHealth, health) // still replaces the waiting one
	h.linkUp()
	sent := h.send()
	var got []string
	for i, it := range sent {
		got = append(got, it.Topic[len(topic()):])
		if seqOf(t, it) != uint64(i+1) {
			t.Fatalf("item %d has seq %d", i, it.Seq)
		}
	}
	if want := []string{"alarm/event", "alarm/event", "device/dev_smoke/event", "system/health"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	if sent[0].ID != first || sent[0].ID != tried.ID || sent[1].ID != second || sent[3].ID == newer {
		t.Fatalf("%+v", sent)
	}
}

func TestRestoreReconcilesWhatACrashLeftOnDisk(t *testing.T) {
	t.Run("an item saved before its snapshot is kept, with its number", func(t *testing.T) {
		h := newHarness(t)
		h.linkUp()
		snap := h.r.Snapshot()
		h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
		tried, _ := h.next()
		h.r = h.restored(snap, h.disk) // the crash came before the snapshot was saved
		h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
		h.linkUp()
		sent := h.send()
		if len(sent) != 2 || sent[0].ID != tried.ID || seqOf(t, sent[0]) != 1 || seqOf(t, sent[1]) != 2 {
			t.Fatalf("%+v", sent)
		}
	})
	t.Run("of two items with the same key, the newer stays", func(t *testing.T) {
		h := newHarness(t)
		h.local(topic("system", "health"), contract.TypeHealth, health)
		older := maps.Clone(h.disk)
		newer, _ := h.local(topic("system", "health"), contract.TypeHealth, contract.HealthData{Links: map[string]string{}})
		maps.Copy(h.disk, older) // the crash came before the older was dropped
		h.restart()
		if len(h.r.queue) != 1 || h.r.queue[0].ID != newer || len(h.disk) != 1 {
			t.Fatalf("queue %+v, disk %v", h.r.queue, h.disk)
		}
	})
	t.Run("an alert the snapshot does not follow goes", func(t *testing.T) {
		h := newHarness(t)
		h.linkUp()
		snap := h.r.Snapshot()
		h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
		sms := appAlert("inc1")
		sms.Channel = contract.ChannelSMS // the GSM's: it only goes up for the record
		kept, _ := h.local(topic("alert", "request"), contract.TypeAlertRequest, sms)
		h.r = h.restored(snap, h.disk)
		if len(h.r.queue) != 1 || h.r.queue[0].ID != kept || len(h.disk) != 1 {
			t.Fatalf("queue %+v, disk %v", h.r.queue, h.disk)
		}
	})
	t.Run("a number saved with its item but not on its own is saved at the restart", func(t *testing.T) {
		h := newHarness(t)
		h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
		h.linkUp()
		it, acts, _ := h.r.Next()
		h.apply(h.disk, acts[:1]) // QueueSave; the crash came before UplinkSave
		h.restart()
		if h.uplink != 1 {
			t.Fatalf("counter %d after the restart", h.uplink)
		}
		h.linkUp()
		h.send()
		h.restart() // the item is gone: the counter alone knows seq 1
		h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
		h.linkUp()
		if sent := h.send(); len(sent) != 1 || sent[0].ID == it.ID || seqOf(t, sent[0]) != 2 {
			t.Fatalf("%+v", sent)
		}
	})
	t.Run("a number of the snapshot is never given again", func(t *testing.T) {
		h := newHarness(t)
		h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
		h.linkUp()
		h.send()
		clear(h.disk) // nothing left on disk: the snapshot alone knows seq 1
		h.restart()
		h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
		h.linkUp()
		if sent := h.send(); len(sent) != 1 || seqOf(t, sent[0]) != 2 {
			t.Fatalf("%+v", sent)
		}
	})
}

// A state saved by a version that kept the queue in the snapshot.
func legacySnapshot(h *harness) ([]byte, []string) {
	h.t.Helper()
	var items []string
	var ids []string
	for _, tp := range []string{"device/dev_smoke/event", "alarm/event"} {
		b, id := h.msg(contract.TypeAlarmArmed, armed)
		prio := PriorityNormal
		if tp == "alarm/event" {
			prio = PriorityAlarm
		}
		items = append(items, fmt.Sprintf(`{"id":%q,"topic":%q,"payload":%s,"prio":%d,"retain":false,"at":"2026-10-20T21:00:00Z"}`,
			id, topic()+tp, b, prio))
		ids = append(ids, id)
	}
	snap := fmt.Sprintf(`{"queue":[%s],"pending":{},"incidents":{},"seen":[],"dropped":2,"tokens":20,"tokens_at":"0001-01-01T00:00:00Z","uplink":7}`,
		strings.Join(items, ","))
	return []byte(snap), ids
}

func TestAQueueSavedInTheSnapshotMovesToItsStore(t *testing.T) {
	h := newHarness(t)
	data, ids := legacySnapshot(h)
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	h.r.Restore(snap)
	acts := h.r.RestoreQueue(nil, 0)
	s, u := of[QueueSave](acts), of[UplinkSave](acts)
	if len(acts) != 3 || len(s) != 2 || s[0].Item.ID != ids[0] || s[1].Item.ID != ids[1] || len(u) != 1 || u[0].Last != 7 {
		t.Fatalf("%+v", acts)
	}
	h.keep(acts)
	if saved, _ := json.Marshal(h.r.Snapshot()); strings.Contains(string(saved), `"queue"`) || strings.Contains(string(saved), `"uplink"`) {
		t.Fatalf("the queue or the counter stay in the snapshot: %s", saved)
	}

	// A crash before the snapshot without the queue was saved: the items are on disk
	// and in the snapshot, and nothing is saved twice.
	again := h.restored(snap, h.disk)
	if len(again.queue) != 2 || len(h.disk) != 2 {
		t.Fatalf("queue %+v, disk %v", again.queue, h.disk)
	}
	h.r = again
	h.linkUp()
	sent := h.send()
	if len(sent) != 2 || sent[0].ID != ids[1] || sent[1].ID != ids[0] || seqOf(t, sent[0]) != 8 {
		t.Fatalf("%+v", sent)
	}
	if h.r.Status(h.now).Dropped != 2 {
		t.Fatal("the count of dropped items is kept")
	}
}
