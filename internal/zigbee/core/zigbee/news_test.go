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

// --- Zigbee2MQTT's own word: a motion sensor back at rest --------------------------

const ieeeMotion = "0x00158d0001000004"

// withMotion adds the profile of a motion sensor that never says "no movement":
// Zigbee2MQTT says it on its own timer.
func withMotion(c *Config) {
	c.Profiles = append(c.Profiles, Profile{
		Name: "aqara.motion.rtcgq11lm", Models: []string{"RTCGQ11LM"},
		Capabilities: map[string]Rule{
			"motion":  {From: "occupancy", Map: map[string]string{"true": "detected", "false": "clear"}, Rest: "clear"},
			"battery": {From: "battery"},
		},
	})
}

// Zigbee2MQTT publishes "no movement" about 90 s after a movement, with the last_seen
// of the movement: no frame came from the sensor. That return to rest is heard, or
// the sensor would stay "detected" and its next movements would change nothing.
func TestZigbee2MQTTsOwnReturnToRestIsHeard(t *testing.T) {
	h := newHarness(t, withMotion)
	h.add("dev_pir", ieeeMotion, "RTCGQ11LM")
	h.now = t0.Add(time.Minute)
	moved := h.now
	if ch := all[Changed](h.heardAt("dev_pir", moved, map[string]any{"occupancy": true, "battery": 90.0})); len(ch) != 2 {
		t.Fatalf("the movement and the battery: %+v", ch)
	}
	h.now = h.now.Add(90 * time.Second)
	out := h.heardAt("dev_pir", moved, map[string]any{"occupancy": false, "battery": 40.0})
	if ch := all[Changed](out); len(ch) != 1 || ch[0].Capability != "motion" || ch[0].Value != "clear" || !ch[0].Notify {
		t.Fatalf("the return to rest is heard, and only it: %+v", out)
	}
	h.now = h.now.Add(time.Minute)
	if ch := all[Changed](h.heardAt("dev_pir", h.now, map[string]any{"occupancy": true})); len(ch) != 1 || ch[0].Value != "detected" {
		t.Fatalf("the next movement is heard: %+v", ch)
	}
	// The cache replayed with the same last_seen raises nothing: a movement already
	// cleared is not seen again.
	h.heardAt("dev_pir", h.now, map[string]any{"occupancy": false})
	h.now = h.now.Add(time.Hour)
	if out := h.heardAt("dev_pir", h.now.Add(-time.Hour), map[string]any{"occupancy": true}); len(all[Changed](out)) != 0 {
		t.Fatalf("a replayed movement: %+v", out)
	}
}

// Zigbee2MQTT keeps the timer that says "no movement" in memory: stopped within the
// 90 s (a crash, an update), it loses it, and its cache replays "detected" with the
// movement's last_seen. Nothing would ever say "no movement": the next movement would
// change nothing, and the stale "detected" would set off a restart while armed. Unheard
// for RestAfter, the sensor goes back to rest by the adapter's own word.
func TestAMotionWhoseReturnToRestIsLostGoesBackToRest(t *testing.T) {
	h := newHarness(t, withMotion)
	h.add("dev_pir", ieeeMotion, "RTCGQ11LM")
	h.now = t0.Add(time.Minute)
	moved := h.now
	h.heardAt("dev_pir", moved, map[string]any{"occupancy": true})
	h.now = h.now.Add(30 * time.Second)
	h.do(Bridge{Now: h.now})
	h.now = h.now.Add(10 * time.Second)
	h.do(Bridge{Now: h.now, Online: true})
	h.heardAt("dev_pir", moved, map[string]any{"occupancy": true}) // the cache
	if out := h.advance(h.n.cfg.RestAfter - 50*time.Second); len(all[Changed](out)) != 0 {
		t.Fatalf("not before RestAfter: %+v", out)
	}
	out := h.advance(10 * time.Second)
	if ch := all[Changed](out); len(ch) != 1 || ch[0].Value != "clear" || !ch[0].Notify || !ch[0].State {
		t.Fatalf("back to rest, announced: %+v", out)
	}
	h.now = h.now.Add(3 * time.Hour)
	if ch := all[Changed](h.heardAt("dev_pir", h.now, map[string]any{"occupancy": true})); len(ch) != 1 || ch[0].Value != "detected" {
		t.Fatalf("the next movement is heard: %+v", ch)
	}
	// A movement that goes on (a message every minute or so) keeps the sensor detected.
	for range 10 {
		h.now = h.now.Add(time.Minute)
		if ch := all[Changed](h.heardAt("dev_pir", h.now, map[string]any{"occupancy": true})); len(ch) != 0 {
			t.Fatalf("still moving: %+v", ch)
		}
	}
}

// The clock is set back (an RTC running fast, corrected by NTP): Zigbee2MQTT's
// last_seen follows it, and the devices' new messages say they were heard before the
// last ones. They are news all the same; for an offline device, after a restart too.
func TestAClockSetBackDoesNotSilenceTheDevices(t *testing.T) {
	for name, offline := range map[string]bool{"online": false, "offline, then a restart": true} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.add("dev_door", ieeeDoor, "MCCGQ11LM")
			h.now = t0.Add(time.Hour)
			h.heardAt("dev_door", h.now, map[string]any{"contact": true})
			if offline {
				h.advance(4 * time.Hour) // its last news is kept, and it is later than the clock set back
				h = h.restarted(h.now.Add(-5 * time.Hour))
			} else {
				h.now = h.now.Add(-30 * time.Minute)
			}
			out := h.heardAt("dev_door", h.now.Add(-time.Second), map[string]any{"contact": false})
			if ch := all[Changed](out); len(ch) != 1 || ch[0].Value != "open" {
				t.Fatalf("the door opening is heard: %+v", out)
			}
			if p := all[Presence](out); offline && (len(p) != 1 || !p[0].Online) {
				t.Fatalf("the door is back online: %+v", out)
			}
		})
	}
}
