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
	if len(ch) != 1 || ch[0] != (Changed{DeviceID: "dev_door", Capability: "contact", Value: "closed", Since: t0, State: true}) {
		t.Fatalf("the first value is a state, not an event: %+v", out)
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
	if p := all[Presence](out); len(p) != 1 || p[0] != (Presence{DeviceID: "dev_siren", Since: t0.Add(time.Hour)}) {
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
