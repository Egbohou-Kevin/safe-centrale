package zigbee

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// --- reports -------------------------------------------------------------------

func TestOnlyChangesAreAnnounced(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")

	out := h.report("dev_door", map[string]any{"contact": true})
	ch := all[Changed](out)
	if len(ch) != 1 || ch[0] != (Changed{DeviceID: "dev_door", Capability: "contact", Value: "closed", Since: t0, State: true, Notify: true}) {
		t.Fatalf("the first value is a state and an event: %+v", out)
	}
	none(t, h.report("dev_door", map[string]any{"contact": true})) // repeated: nothing
	h.now = h.now.Add(time.Minute)
	out = h.report("dev_door", map[string]any{"contact": false})
	ch = all[Changed](out)
	if len(ch) != 1 || !ch[0].Notify || !ch[0].State || ch[0].Value != "open" || !ch[0].Since.Equal(t0.Add(time.Minute)) {
		t.Fatalf("%+v", out)
	}
	// A value no profile knows, a missing field, a wrong type: nothing.
	none(t, h.report("dev_door", map[string]any{"contact": "maybe", "tamper": 3.0, "voltage": 3000.0}))
}

// Found with the virtual kit: a door paired while closed but silent until the
// intrusion says "open" first. The alarm must hear it.
func TestAFirstOpeningIsAnnounced(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	ch := all[Changed](h.report("dev_door", map[string]any{"contact": false}))
	if len(ch) != 1 || ch[0].Value != "open" || !ch[0].Notify {
		t.Fatalf("%+v", ch)
	}
}

func TestNumbersAreScaledAndAnnouncedWhenTheyMoveEnough(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	out := h.report("dev_door", map[string]any{"battery": 91.0, "linkquality": 255.0})
	ch := all[Changed](out)
	if len(ch) != 2 || ch[0].Capability != "battery" || ch[0].Value != "91" || !ch[0].Notify || !ch[0].Numeric ||
		ch[1].Capability != "link" || ch[1].Value != "100" || !ch[1].Notify {
		t.Fatalf("the supervision needs the first number: %+v", out)
	}
	none(t, h.report("dev_door", map[string]any{"battery": 87.0, "linkquality": 215.0})) // -4 and -16: too little
	out = h.report("dev_door", map[string]any{"battery": 86.0, "linkquality": 128.0})
	ch = all[Changed](out)
	if len(ch) != 2 || ch[0].Value != "86" || ch[1].Value != "50" {
		t.Fatalf("%+v", out)
	}
	out = h.report("dev_door", map[string]any{"battery": 140.0})
	if ch := all[Changed](out); len(ch) != 1 || ch[0].Value != "100" {
		t.Fatalf("clamped to 100: %+v", out)
	}
}

func TestRetainedMessagesRestoreTheStateWithoutEvents(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	h.add("dev_kp", ieeeKeypad, "KP-1")
	h.report("dev_door", map[string]any{"contact": true})

	out := h.retained("dev_door", map[string]any{"contact": false, "battery": 50.0})
	for _, ch := range all[Changed](out) {
		if ch.Notify || !ch.State {
			t.Fatalf("an old value replayed is never an event: %+v", out)
		}
	}
	if len(all[Changed](out)) != 2 {
		t.Fatalf("%+v", out)
	}
	none(t, h.retained("dev_kp", map[string]any{"action": "emergency"})) // an old action is not replayed
}

func TestKeypadAndPanicActionsAreEventsEveryTime(t *testing.T) {
	h := newHarness(t)
	h.add("dev_kp", ieeeKeypad, "KP-1")
	for range 2 {
		out := h.report("dev_kp", map[string]any{"action": "disarm", "action_code": "482915"})
		ch := all[Changed](out)
		if len(ch) != 1 || ch[0].Capability != "keypad" || ch[0].Value != "disarm" || ch[0].Code != "482915" || !ch[0].Notify || ch[0].State {
			t.Fatalf("%+v", out)
		}
	}
	out := h.report("dev_kp", map[string]any{"action": "emergency"})
	if ch := all[Changed](out); len(ch) != 1 || ch[0].Capability != "panic" || ch[0].Value != "pressed" || ch[0].Code != "" {
		t.Fatalf("%+v", out)
	}
	none(t, h.report("dev_kp", map[string]any{"action": ""}))          // Zigbee2MQTT clears the action
	none(t, h.report("dev_kp", map[string]any{"action": "arm_night"})) // not in the profile
	if len(h.n.Snapshot().States["dev_kp"]) != 0 {
		t.Fatal("an action is not a state")
	}
}

// A key dates from when Zigbee2MQTT heard it: one pressed while the adapter was down
// reaches us late, and must look as old as it is. Never later than now.
func TestAnActionDatesFromWhenItWasHeard(t *testing.T) {
	h := newHarness(t)
	h.add("dev_kp", ieeeKeypad, "KP-1")
	for heard, want := range map[time.Time]time.Time{
		h.now.Add(-2 * time.Hour): h.now.Add(-2 * time.Hour),
		h.now.Add(time.Hour):      h.now,
		{}:                        h.now,
	} {
		out := h.do(Report{Now: h.now, Name: "dev_kp", Fields: map[string]any{"action": "disarm"}, Heard: heard})
		if ch := all[Changed](out); len(ch) != 1 || !ch[0].Since.Equal(want) {
			t.Fatalf("heard %v: %+v", heard, out)
		}
	}
}

func TestMessagesAreFoundByIdOrByAddressAndOthersIgnored(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	if ch := all[Changed](h.report(ieeeDoor, map[string]any{"contact": true})); len(ch) != 1 || ch[0].DeviceID != "dev_door" {
		t.Fatal("before the rename, Zigbee2MQTT names the device by its address")
	}
	if _, ok := find[Ignored](h.report("0xdeadbeef", map[string]any{"contact": true})); !ok {
		t.Fatal("a stranger is ignored")
	}
}

// --- presence ------------------------------------------------------------------

func TestASilentDeviceGoesOfflineAndComesBack(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	if !h.n.NextWake().Equal(t0.Add(time.Hour)) {
		t.Fatalf("next wake %v: the siren's profile allows one hour", h.n.NextWake())
	}
	out := h.advance(time.Hour)
	if p := all[Presence](out); len(p) != 1 || p[0] != (Presence{DeviceID: "dev_siren", Since: t0.Add(time.Hour), Heard: t0}) {
		t.Fatalf("%+v", out)
	}
	h.now = t0.Add(2 * time.Hour)
	h.report("dev_door", map[string]any{"contact": true}) // the door speaks: its silence restarts
	none(t, h.advance(4*time.Hour-time.Second))
	if p := all[Presence](h.advance(time.Second)); len(p) != 1 || p[0].DeviceID != "dev_door" {
		t.Fatal("four hours after its last message")
	}
	if st := h.n.Status(); !slices.Equal(st.Offline, []string{"dev_door", "dev_siren"}) {
		t.Fatalf("%+v", st)
	}
	// A retained message is old news: the siren stays offline.
	if _, ok := find[Presence](h.retained("dev_siren", map[string]any{"alarm": "OFF"})); ok {
		t.Fatal("a retained message does not bring a device back")
	}
	out = h.report("dev_siren", map[string]any{"alarm": "OFF"})
	if p := all[Presence](out); len(p) != 1 || !p[0].Online {
		t.Fatalf("%+v", out)
	}
	if !h.n.NextWake().Equal(h.now.Add(time.Hour)) {
		t.Fatalf("next wake %v", h.n.NextWake())
	}
}

// An offline device carries its last news, to the minute: from then on, its door
// is not watched. It stays fixed while the device is offline, and a device that
// answers has none: when it last spoke moves with every door and every passer-by.
func TestAnOfflineDeviceSaysWhenItWasLastHeard(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM") // joining is news from it
	h.now = t0.Add(2*time.Hour + 47*time.Minute + 31*time.Second)
	h.report("dev_door", map[string]any{"contact": true})
	out := h.advance(4 * time.Hour)
	if p := all[Presence](out); len(p) != 1 || p[0] != (Presence{DeviceID: "dev_door", Since: h.now, Heard: t0.Add(2*time.Hour + 47*time.Minute)}) {
		t.Fatalf("%+v", out)
	}
	h.now = h.now.Add(time.Hour)
	out = h.report("dev_door", map[string]any{"contact": true})
	if p := all[Presence](out); len(p) != 1 || !p[0].Online || !p[0].Heard.IsZero() {
		t.Fatalf("back online, with no time but its return: %+v", out)
	}

	// A device that leaves the network carries its last news too.
	h.now = h.now.Add(10*time.Minute + 59*time.Second)
	h.report("dev_door", map[string]any{"contact": true})
	spoke := h.now
	h.now = h.now.Add(time.Minute)
	if p := all[Presence](h.do(Left{Now: h.now, IEEE: ieeeDoor})); len(p) != 1 || !p[0].Heard.Equal(spoke.Truncate(time.Minute)) {
		t.Fatalf("%+v", p)
	}
}

// After a restart, a device that said nothing since goes offline with no last news:
// the restart is no news from it. One that spoke since has its own.
func TestAfterARestartADeviceSilentSinceHasNoLastNews(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	h.add("dev_back", "0x00158d0001000009", "MCCGQ11LM")
	var s Snapshot
	raw, _ := json.Marshal(h.n.Snapshot())
	json.Unmarshal(raw, &s)

	again := newHarness(t)
	again.n.Restore(s)
	again.now = t0.Add(time.Hour)
	again.do(Tick{Now: again.now}) // the restart: both are silent since
	again.now = t0.Add(90*time.Minute + 20*time.Second)
	again.report("dev_back", map[string]any{"contact": true})

	out := again.advance(3*time.Hour + 39*time.Minute + 40*time.Second) // t0+5h
	if p := all[Presence](out); len(p) != 1 || p[0] != (Presence{DeviceID: "dev_door", Since: again.now}) {
		t.Fatalf("%+v", out)
	}
	out = again.advance(30*time.Minute + 20*time.Second)
	if p := all[Presence](out); len(p) != 1 || p[0] != (Presence{DeviceID: "dev_back", Since: again.now, Heard: t0.Add(90 * time.Minute)}) {
		t.Fatalf("%+v", out)
	}
}

func TestADeviceThatLeavesIsOffline(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	if p := all[Presence](h.do(Left{Now: h.now, IEEE: ieeeDoor})); len(p) != 1 || p[0].Online {
		t.Fatal("offline at once")
	}
	none(t, h.do(Left{Now: h.now, IEEE: ieeeDoor}))
	none(t, h.do(Left{Now: h.now, IEEE: "0xunknown"}))
}

func TestAfterARestartSilenceCountsFromTheFirstInput(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	raw, _ := json.Marshal(h.n.Snapshot())
	if strings.Contains(string(raw), "seen") {
		t.Fatal("when a device last spoke is not saved")
	}
	var s Snapshot
	json.Unmarshal(raw, &s)

	again := newHarness(t)
	again.n.Restore(s)
	if !again.n.NextWake().Equal(time.Unix(0, 0)) {
		t.Fatalf("next wake %v: a tick right away starts the count", again.n.NextWake())
	}
	again.now = t0.Add(10 * time.Hour) // restarted much later
	none(t, again.do(Tick{Now: again.now}))
	if !again.n.NextWake().Equal(again.now.Add(4 * time.Hour)) {
		t.Fatalf("next wake %v", again.n.NextWake())
	}
}
