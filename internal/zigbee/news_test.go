package zigbee

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// --- Zigbee2MQTT replaying its cache when it starts -----------------------------

// heardAt is a live message Zigbee2MQTT says it heard at heard (its last_seen).
func (h *harness) heardAt(name string, heard time.Time, fields map[string]any) []Action {
	return h.do(Report{Now: h.now, Name: name, Fields: fields, Heard: heard})
}

// restarted is the same network after a restart of the adapter, at now.
func (h *harness) restarted(now time.Time) *harness {
	h.t.Helper()
	raw, err := json.Marshal(h.n.Snapshot())
	if err != nil {
		h.t.Fatal(err)
	}
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		h.t.Fatal(err)
	}
	again := newHarness(h.t)
	again.n.Restore(s)
	again.now = now
	return again
}

// A door whose battery died is offline. When Zigbee2MQTT starts again (a power cut),
// it publishes its cache of every device, not retained, with the old last_seen: that
// is no news of the door, which stays offline, and its old values are no opening.
func TestACacheReplayedAtZigbee2MQTTsStartDoesNotBringADeadDoorBack(t *testing.T) {
	for name, restart := range map[string]bool{"zigbee2mqtt alone": false, "the whole hub": true} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.add("dev_door", ieeeDoor, "MCCGQ11LM")
			h.now = t0.Add(time.Minute)
			last := h.now.Add(-time.Second)
			h.heardAt("dev_door", last, map[string]any{"contact": true})
			if p := all[Presence](h.advance(4 * time.Hour)); len(p) != 1 || p[0].Online {
				t.Fatalf("the dead door goes offline: %+v", p)
			}
			h.now = h.now.Add(44 * time.Hour)
			if restart {
				h = h.restarted(h.now)
			}
			out := h.heardAt("dev_door", last, map[string]any{"contact": false})
			if len(all[Presence](out)) != 0 || len(all[Changed](out)) != 0 {
				t.Fatalf("the replay is no news: %+v", out)
			}
			if !slices.Contains(h.n.Status().Offline, "dev_door") {
				t.Fatal("the door stays offline")
			}
			// Real news brings it back.
			out = h.heardAt("dev_door", h.now.Add(-time.Second), map[string]any{"contact": false})
			if p := all[Presence](out); len(p) != 1 || !p[0].Online || len(all[Changed](out)) != 1 {
				t.Fatalf("the door speaks again: %+v", out)
			}
		})
	}
}

// A device online when Zigbee2MQTT restarts: the cache replayed may be older than what
// the adapter heard since (Zigbee2MQTT saves it every few minutes). Its old "open" is
// not announced as an opening.
func TestAnOldValueReplayedByZigbee2MQTTIsNotAnnounced(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	h.now = t0.Add(time.Minute)
	h.heardAt("dev_door", h.now, map[string]any{"contact": false})
	h.now = h.now.Add(time.Minute)
	h.heardAt("dev_door", h.now, map[string]any{"contact": true})
	h.now = h.now.Add(time.Minute)
	if out := h.heardAt("dev_door", t0.Add(time.Minute), map[string]any{"contact": false}); len(all[Changed](out)) != 0 {
		t.Fatalf("an old value: %+v", out)
	}
	if h.n.Snapshot().States["dev_door"]["contact"].Value != "closed" {
		t.Fatal("the state the door last told is kept")
	}
	// A message that does not say when it was heard cannot be judged: it counts.
	if out := h.report("dev_door", map[string]any{"contact": false}); len(all[Changed](out)) != 1 {
		t.Fatalf("%+v", out)
	}
}
