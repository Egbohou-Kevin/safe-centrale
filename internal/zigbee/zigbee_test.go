package zigbee

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

const (
	ieeeDoor   = "0x00158d0001000001"
	ieeeSiren  = "0x00158d0001000002"
	ieeeKeypad = "0x00158d0001000003"
)

func contactProfile() Profile {
	return Profile{
		Name: "aqara.contact.mccgq11lm", Models: []string{"MCCGQ11LM"},
		Capabilities: map[string]Rule{
			"contact": {From: "contact", Map: map[string]string{"true": "closed", "false": "open"}},
			"battery": {From: "battery"},
			"link":    {From: "linkquality", Max: 255, Step: 20},
			"tamper":  {From: "tamper", Map: map[string]string{"true": "triggered", "false": "clear"}},
		},
	}
}

func sirenProfile() Profile {
	return Profile{
		Name: "heiman.siren.hs2wd", Models: []string{"HS2WD-E"},
		Capabilities: map[string]Rule{
			"siren": {From: "alarm", Map: map[string]string{"ON": "on", "OFF": "off"}},
		},
		Commands: map[string]ProfileCommand{
			"siren.on": {
				Payload: map[string]any{"warning": map[string]any{"mode": "burglar", "duration": "{duration_s}", "steps": []any{"{duration_s}", 1}}},
				Confirm: &Expect{Capability: "siren", Value: "on"},
			},
			"siren.off":       {Payload: map[string]any{"warning": map[string]any{"mode": "stop"}}, Confirm: &Expect{Capability: "siren", Value: "off"}},
			"device.identify": {Payload: map[string]any{"identify": "identify"}},
		},
		OfflineAfter: time.Hour,
	}
}

func keypadProfile() Profile {
	return Profile{
		Name: "tuya.keypad.example", Models: []string{"KP-1"},
		Capabilities: map[string]Rule{
			"keypad":  {From: "action", Map: map[string]string{"arm_all_zones": "arm", "disarm": "disarm"}, Momentary: true, CodeFrom: "action_code"},
			"panic":   {From: "action", Map: map[string]string{"emergency": "pressed"}, Momentary: true},
			"battery": {From: "battery"},
		},
	}
}

type harness struct {
	t   *testing.T
	n   *Network
	now time.Time
	ids []string // ids newID hands out, in order
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, now: t0}
	cfg := DefaultConfig([]Profile{contactProfile(), sirenProfile(), keypadProfile()})
	for _, m := range mutate {
		m(&cfg)
	}
	n, err := New(cfg, func() string {
		if len(h.ids) == 0 {
			return fmt.Sprintf("dev_%d", time.Now().UnixNano())
		}
		id := h.ids[0]
		h.ids = h.ids[1:]
		return id
	})
	if err != nil {
		t.Fatal(err)
	}
	h.n = n
	return h
}

func (h *harness) do(in Input) []Action {
	h.t.Helper()
	return h.n.Handle(in)
}

func (h *harness) advance(d time.Duration) []Action {
	h.now = h.now.Add(d)
	return h.do(Tick{Now: h.now})
}

func (h *harness) report(name string, fields map[string]any) []Action {
	return h.do(Report{Now: h.now, Name: name, Fields: fields})
}

func (h *harness) retained(name string, fields map[string]any) []Action {
	return h.do(Report{Now: h.now, Name: name, Fields: fields, Retained: true})
}

// add pairs a device of the given model as id.
func (h *harness) add(id, ieee, model string) {
	h.t.Helper()
	h.ids = append(h.ids, id)
	h.do(Pair{Now: h.now, Ref: "pair-1", For: 2 * time.Minute})
	out := h.do(Interviewed{Now: h.now, IEEE: ieee, Model: model, OK: true})
	if _, ok := find[Added](out); !ok {
		h.t.Fatalf("%s was not added: %+v", model, out)
	}
	h.do(Pair{Now: h.now, Ref: "pair-1"})
}

func find[T Action](as []Action) (T, bool) {
	for _, a := range as {
		if v, ok := a.(T); ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

func all[T Action](as []Action) []T {
	var out []T
	for _, a := range as {
		if v, ok := a.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

func none(t *testing.T, as []Action) {
	t.Helper()
	if len(as) != 0 {
		t.Fatalf("want nothing, got %+v", as)
	}
}

// --- configuration -------------------------------------------------------------

func TestDefaultsAndValidation(t *testing.T) {
	d := DefaultConfig(nil)
	if d.ConfirmWait != 3*time.Second || d.Attempts != 3 || d.OfflineAfter != 4*time.Hour || d.PairMax != 10*time.Minute || d.Step != 5 {
		t.Fatalf("%+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Config){
		"no confirm wait": func(c *Config) { c.ConfirmWait = 0 },
		"no attempt":      func(c *Config) { c.Attempts = 0 },
		"no offline":      func(c *Config) { c.OfflineAfter = 0 },
		"no pair max":     func(c *Config) { c.PairMax = 0 },
		"no step":         func(c *Config) { c.Step = 0 },
		"same name":       func(c *Config) { p := contactProfile(); p.Models = []string{"X"}; c.Profiles = append(c.Profiles, p) },
		"same model":      func(c *Config) { p := sirenProfile(); p.Name = "other"; c.Profiles = append(c.Profiles, p) },
		"no name":         func(c *Config) { c.Profiles[0].Name = "" },
		"no model":        func(c *Config) { c.Profiles[0].Models = nil },
		"reads nothing":   func(c *Config) { c.Profiles[0].Capabilities = nil },
		"negative offline": func(c *Config) {
			c.Profiles[0].OfflineAfter = -time.Second
		},
		"no field":       func(c *Config) { c.Profiles[0].Capabilities["contact"] = Rule{} },
		"numeric action": func(c *Config) { c.Profiles[0].Capabilities["panic"] = Rule{From: "action", Momentary: true} },
		"code on a state": func(c *Config) {
			c.Profiles[0].Capabilities["x"] = Rule{From: "x", Map: map[string]string{"a": "b"}, CodeFrom: "c"}
		},
		"negative scale": func(c *Config) { c.Profiles[0].Capabilities["link"] = Rule{From: "linkquality", Max: -1} },
		"negative step":  func(c *Config) { c.Profiles[0].Capabilities["link"] = Rule{From: "linkquality", Step: -1} },
		"maps to nothing": func(c *Config) {
			c.Profiles[0].Capabilities["contact"] = Rule{From: "contact", Map: map[string]string{"true": ""}}
		},
		"empty command": func(c *Config) { c.Profiles[1].Commands["x"] = ProfileCommand{} },
		"confirm on nothing": func(c *Config) {
			c.Profiles[1].Commands["x"] = ProfileCommand{Payload: map[string]any{"a": 1}, Confirm: &Expect{Capability: "nope", Value: "on"}}
		},
		"confirm an action": func(c *Config) {
			c.Profiles[2].Commands = map[string]ProfileCommand{"x": {Payload: map[string]any{"a": 1}, Confirm: &Expect{Capability: "panic", Value: "pressed"}}}
		},
	}
	for name, m := range bad {
		c := DefaultConfig([]Profile{contactProfile(), sirenProfile(), keypadProfile()})
		m(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(DefaultConfig(nil), nil); err == nil {
		t.Error("newID is required")
	}
	if _, err := New(Config{}, func() string { return "x" }); err == nil {
		t.Error("an invalid configuration is refused")
	}
}

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

// --- commands ------------------------------------------------------------------

func TestACommandIsSentAndConfirmedByTheDevice(t *testing.T) {
	h := newHarness(t)
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	out := h.do(Command{Now: h.now, Ref: "c1", DeviceID: "dev_siren", Name: "siren.on", DurationS: 300, Exp: h.now.Add(30 * time.Second)})
	send, ok := find[Send](out)
	want := map[string]any{"warning": map[string]any{"mode": "burglar", "duration": 300, "steps": []any{300, 1}}}
	if !ok || send.IEEE != ieeeSiren || !reflect.DeepEqual(send.Payload, want) {
		t.Fatalf("%+v", out)
	}
	if acks := all[Ack](out); len(acks) != 1 || acks[0] != (Ack{Ref: "c1", DeviceID: "dev_siren", Status: AckAccepted}) {
		t.Fatalf("%+v", out)
	}
	if !h.n.NextWake().Equal(t0.Add(3 * time.Second)) {
		t.Fatalf("next wake %v", h.n.NextWake())
	}
	out = h.report("dev_siren", map[string]any{"alarm": "ON"})
	if acks := all[Ack](out); len(acks) != 1 || acks[0].Status != AckDone || acks[0].Ref != "c1" {
		t.Fatalf("%+v", out)
	}
	// Already on: a second siren.on is confirmed by the next report even without change.
	h.do(Command{Now: h.now, Ref: "c2", DeviceID: "dev_siren", Name: "siren.on", DurationS: 60})
	if acks := all[Ack](h.report("dev_siren", map[string]any{"alarm": "ON"})); len(acks) != 1 || acks[0].Ref != "c2" {
		t.Fatal("the state it waits for, changed or not")
	}
	// A retained message is no confirmation.
	h.do(Command{Now: h.now, Ref: "c3", DeviceID: "dev_siren", Name: "siren.off"})
	if _, ok := find[Ack](h.retained("dev_siren", map[string]any{"alarm": "OFF"})); ok {
		t.Fatal("an old state confirms nothing")
	}
	if _, ok := find[Ack](h.report("dev_siren", map[string]any{"linkquality": 3.0})); ok {
		t.Fatal("a message without the field confirms nothing")
	}
}

func TestAnUnconfirmedCommandIsSentAgainThenFails(t *testing.T) {
	h := newHarness(t)
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	h.do(Command{Now: h.now, Ref: "c1", DeviceID: "dev_siren", Name: "siren.off"})
	for i := range 2 {
		out := h.advance(3 * time.Second)
		if _, ok := find[Send](out); !ok {
			t.Fatalf("attempt %d: %+v", i+2, out)
		}
	}
	out := h.advance(3 * time.Second)
	if acks := all[Ack](out); len(acks) != 1 || acks[0].Status != AckFailed || acks[0].Reason != ReasonNoConfirmation {
		t.Fatalf("%+v", out)
	}
	if _, ok := find[Send](out); ok {
		t.Fatal("three attempts in all")
	}
	none(t, h.advance(time.Minute))
}

func TestACommandPastItsExpiryIsNotCarriedOut(t *testing.T) {
	h := newHarness(t)
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	out := h.do(Command{Now: h.now, Ref: "c1", DeviceID: "dev_siren", Name: "siren.on", DurationS: 60, Exp: h.now})
	if len(out) != 1 || out[0] != (Ack{Ref: "c1", DeviceID: "dev_siren", Status: AckExpired}) {
		t.Fatalf("%+v", out)
	}
	// Expiry reached while waiting for the confirmation.
	h.do(Command{Now: h.now, Ref: "c2", DeviceID: "dev_siren", Name: "siren.on", DurationS: 60, Exp: h.now.Add(5 * time.Second)})
	h.advance(3 * time.Second) // sent again
	out = h.advance(3 * time.Second)
	if acks := all[Ack](out); len(acks) != 1 || acks[0].Status != AckExpired || acks[0].Ref != "c2" {
		t.Fatalf("%+v", out)
	}
}

func TestCommandsThatCannotBeCarriedOut(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	if out := h.do(Command{Now: h.now, Ref: "c1", DeviceID: "dev_nope", Name: "siren.on"}); len(out) != 1 || out[0].(Ack).Reason != ReasonUnknownDevice {
		t.Fatalf("%+v", out)
	}
	if out := h.do(Command{Now: h.now, Ref: "c2", DeviceID: "dev_door", Name: "siren.on"}); len(out) != 1 || out[0].(Ack).Reason != ReasonUnsupported {
		t.Fatalf("%+v", out)
	}
	// Without a confirmation in the profile, a command is done once sent.
	out := h.do(Command{Now: h.now, Ref: "c3", DeviceID: "dev_siren", Name: "device.identify"})
	if acks := all[Ack](out); len(acks) != 2 || acks[0].Status != AckAccepted || acks[1].Status != AckDone {
		t.Fatalf("%+v", out)
	}
}

func TestANewCommandSupersedesTheOneWaiting(t *testing.T) {
	h := newHarness(t)
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	h.do(Command{Now: h.now, Ref: "on", DeviceID: "dev_siren", Name: "siren.on", DurationS: 60})
	out := h.do(Command{Now: h.now, Ref: "off", DeviceID: "dev_siren", Name: "siren.off"})
	acks := all[Ack](out)
	if len(acks) != 2 || acks[0] != (Ack{Ref: "on", DeviceID: "dev_siren", Status: AckFailed, Reason: ReasonSuperseded}) || acks[1].Ref != "off" {
		t.Fatalf("%+v", out)
	}
	if h.n.Snapshot().Pending["dev_siren"].Ref != "off" {
		t.Fatal("one command at a time per device")
	}
}

// --- pairing and removal -------------------------------------------------------

func TestPairingAcceptsAModelWithAProfile(t *testing.T) {
	h := newHarness(t)
	h.ids = []string{"dev_k7p2"}
	out := h.do(Pair{Now: h.now, Ref: "p1", For: 2 * time.Minute})
	if len(out) != 1 || out[0] != (PermitJoin{Seconds: 120}) || !h.n.Status().Pairing {
		t.Fatalf("%+v", out)
	}
	out = h.do(Interviewed{Now: h.now, IEEE: ieeeDoor, Model: "MCCGQ11LM", OK: true})
	want := Device{ID: "dev_k7p2", IEEE: ieeeDoor, Model: "MCCGQ11LM", Profile: "aqara.contact.mccgq11lm",
		Capabilities: []string{"battery", "contact", "link", "tamper"}}
	if len(out) != 3 || out[0] != (Rename{IEEE: ieeeDoor, To: "dev_k7p2"}) ||
		!reflect.DeepEqual(out[1], Added{Ref: "p1", Device: want}) ||
		!reflect.DeepEqual(out[2], RegistryChanged{Devices: []Device{want}}) {
		t.Fatalf("%+v", out)
	}
	if _, ok := find[Ignored](h.do(Interviewed{Now: h.now, IEEE: ieeeDoor, Model: "MCCGQ11LM", OK: true})); !ok {
		t.Fatal("questioned again: nothing changes")
	}
	// The window closes by itself.
	if !h.n.NextWake().Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("next wake %v", h.n.NextWake())
	}
	if out := h.advance(2 * time.Minute); len(out) != 1 || out[0] != (PermitJoin{}) || h.n.Status().Pairing {
		t.Fatalf("%+v", out)
	}
}

func TestPairingIsBoundedAndCanBeClosed(t *testing.T) {
	h := newHarness(t)
	if out := h.do(Pair{Now: h.now, Ref: "p", For: time.Hour}); out[0] != (PermitJoin{Seconds: 600}) {
		t.Fatalf("%+v", out)
	}
	if out := h.do(Pair{Now: h.now, Ref: "p"}); len(out) != 1 || out[0] != (PermitJoin{}) || h.n.Status().Pairing {
		t.Fatalf("%+v", out)
	}
	none(t, h.advance(time.Hour))
}

func TestDevicesWeCannotWatchAreRefused(t *testing.T) {
	h := newHarness(t)
	out := h.do(Interviewed{Now: h.now, IEEE: "0x01", Model: "MCCGQ11LM", OK: true})
	if !reflect.DeepEqual(out, []Action{Expel{IEEE: "0x01"}, Rejected{Model: "MCCGQ11LM", Reason: RejectNotPairing}}) {
		t.Fatalf("outside a pairing window: %+v", out)
	}
	h.do(Pair{Now: h.now, Ref: "p1", For: time.Minute})
	out = h.do(Interviewed{Now: h.now, IEEE: "0x02", Model: "WXKG01LM", OK: true})
	if !reflect.DeepEqual(out, []Action{Expel{IEEE: "0x02"}, Rejected{Ref: "p1", Model: "WXKG01LM", Reason: RejectUnknownModel}}) {
		t.Fatalf("no profile: %+v", out)
	}
	out = h.do(Interviewed{Now: h.now, IEEE: "0x03", OK: false})
	if !reflect.DeepEqual(out, []Action{Expel{IEEE: "0x03"}, Rejected{Ref: "p1", Reason: RejectInterviewFailed}}) {
		t.Fatalf("model unknown: %+v", out)
	}
	if h.n.Status().Devices != 0 {
		t.Fatal("nothing registered")
	}
}

func TestDeviceIdsAreNeverGivenTwice(t *testing.T) {
	h := newHarness(t)
	h.add("dev_a", ieeeDoor, "MCCGQ11LM")
	h.do(Remove{Now: h.now, Ref: "r", DeviceID: "dev_a"})
	h.ids = []string{"dev_a", "", "dev_b"} // retired, empty, then free
	h.do(Pair{Now: h.now, Ref: "p", For: time.Minute})
	out := h.do(Interviewed{Now: h.now, IEEE: ieeeSiren, Model: "HS2WD-E", OK: true})
	if a, _ := find[Added](out); a.Device.ID != "dev_b" {
		t.Fatalf("%+v", out)
	}
	// An id generator that only repeats itself: the device is refused rather than
	// given a taken id.
	h.ids = nil
	stuck := newHarness(t)
	stuck.add("dev_x", ieeeDoor, "MCCGQ11LM")
	stuck.n.newID = func() string { return "dev_x" }
	stuck.do(Pair{Now: stuck.now, Ref: "p", For: time.Minute})
	out = stuck.do(Interviewed{Now: stuck.now, IEEE: ieeeSiren, Model: "HS2WD-E", OK: true})
	if r, ok := find[Rejected](out); !ok || r.Reason != RejectInterviewFailed {
		t.Fatalf("%+v", out)
	}
}

func TestRemovalForgetsTheDevice(t *testing.T) {
	h := newHarness(t)
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	h.report("dev_siren", map[string]any{"alarm": "OFF"})
	h.do(Command{Now: h.now, Ref: "c1", DeviceID: "dev_siren", Name: "siren.on", DurationS: 60})
	out := h.do(Remove{Now: h.now, Ref: "r1", DeviceID: "dev_siren"})
	if len(out) != 4 || out[0] != (Expel{IEEE: ieeeSiren}) ||
		out[1] != (Ack{Ref: "c1", DeviceID: "dev_siren", Status: AckFailed, Reason: ReasonRemoved}) {
		t.Fatalf("%+v", out)
	}
	if r := out[2].(Removed); r.Ref != "r1" || r.Device.ID != "dev_siren" || out[3].(RegistryChanged).Devices == nil || len(out[3].(RegistryChanged).Devices) != 0 {
		t.Fatalf("%+v", out)
	}
	s := h.n.Snapshot()
	if len(s.Devices) != 0 || len(s.States) != 0 || len(s.Pending) != 0 || !s.Retired["dev_siren"] {
		t.Fatalf("%+v", s)
	}
	if _, ok := find[Ignored](h.do(Remove{Now: h.now, Ref: "r2", DeviceID: "dev_siren"})); !ok {
		t.Fatal("removed twice")
	}
	none(t, h.advance(5*time.Hour)) // no presence watch, no command left
}

// --- state ---------------------------------------------------------------------

func TestTheStateSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	h.report("dev_door", map[string]any{"contact": false})
	h.do(Bridge{Now: h.now, Online: true})
	h.do(Command{Now: h.now, Ref: "c1", DeviceID: "dev_siren", Name: "siren.on", DurationS: 60})
	raw, err := json.Marshal(h.n.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	again := newHarness(t)
	again.n.Restore(s)
	again.now = h.now
	if !reflect.DeepEqual(again.n.Snapshot(), h.n.Snapshot()) || !again.n.Status().Bridge || again.n.Status().Devices != 2 {
		t.Fatalf("%+v", again.n.Snapshot())
	}
	none(t, again.report("dev_door", map[string]any{"contact": false})) // known: no change
	if acks := all[Ack](again.report("dev_siren", map[string]any{"alarm": "ON"})); len(acks) != 1 || acks[0].Ref != "c1" {
		t.Fatal("the command waiting survives")
	}
	// The copy is a copy.
	snap := again.n.Snapshot()
	snap.States["dev_door"]["contact"] = State{Value: "x"}
	if again.n.Snapshot().States["dev_door"]["contact"].Value != "open" {
		t.Fatal("Snapshot must not share its maps")
	}
	devs := again.n.Devices()
	devs[0].Capabilities[0] = "x"
	if again.n.Devices()[0].Capabilities[0] == "x" {
		t.Fatal("Devices must not share its lists")
	}
}

func TestBridgeStatus(t *testing.T) {
	h := newHarness(t)
	if h.n.Status().Bridge {
		t.Fatal("unknown at first")
	}
	h.do(Bridge{Now: h.now, Online: true})
	if !h.n.Status().Bridge {
		t.Fatal("online")
	}
	h.do(Bridge{Now: h.now, Online: false})
	if h.n.Status().Bridge {
		t.Fatal("offline")
	}
}

func TestAMessageFromADeviceWhoseProfileIsGoneIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	s := h.n.Snapshot()
	other := newHarness(t, func(c *Config) { c.Profiles = []Profile{sirenProfile()} })
	other.n.Restore(s)
	if _, ok := find[Ignored](other.report("dev_door", map[string]any{"contact": true})); !ok {
		t.Fatal("ignored and logged")
	}
}
