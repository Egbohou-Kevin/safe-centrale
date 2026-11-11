package zigbee

import (
	"encoding/json"
	"fmt"
	"reflect"
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
		"confirm a raw value": func(c *Config) {
			c.Profiles[1].Commands["x"] = ProfileCommand{Payload: map[string]any{"a": 1}, Confirm: &Expect{Capability: "siren", Value: "ON"}}
		},
		"confirm a number out of range": func(c *Config) {
			c.Profiles[0].Commands = map[string]ProfileCommand{"x": {Payload: map[string]any{"a": 1}, Confirm: &Expect{Capability: "battery", Value: "101"}}}
		},
		"rest never read": func(c *Config) {
			c.Profiles[0].Capabilities["tamper"] = Rule{From: "tamper", Map: map[string]string{"true": "triggered"}, Rest: "clear"}
		},
		"rest of an action": func(c *Config) {
			c.Profiles[2].Capabilities["panic"] = Rule{From: "action", Map: map[string]string{"emergency": "pressed"}, Momentary: true, Rest: "pressed"}
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
	want := h.n.Snapshot()
	want.Bridge = false // Zigbee2MQTT running before is no evidence that it runs now
	if !reflect.DeepEqual(again.n.Snapshot(), want) || again.n.Status().Bridge || again.n.Status().Devices != 2 {
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

// A device paired before its profile changed (the catalogue's siren, once listed
// with "link" only) is what its profile says now, from the next start.
func TestARestartTakesTheCapabilitiesFromTheProfile(t *testing.T) {
	h := newHarness(t)
	stale := Device{ID: "dev_siren", IEEE: ieeeSiren, Model: "HS2WD-E", Profile: "heiman.siren.hs2wd", Capabilities: []string{"link"}}
	h.n.Restore(Snapshot{Devices: map[string]Device{"dev_siren": stale}})
	if got := h.n.Devices()[0].Capabilities; !reflect.DeepEqual(got, []string{"siren"}) {
		t.Fatalf("%v", got)
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

// After a power cut, the broker hands over the "online" Zigbee2MQTT said before it:
// Zigbee2MQTT may be unable to start (a stick that fails), and then never says
// otherwise. It is asked whether it runs, and counts as online once it answers; not
// before, nor when it does not answer.
func TestARetainedOnlineIsCheckedWithZigbee2MQTT(t *testing.T) {
	h := newHarness(t)
	h.add("dev_kp", ieeeKeypad, "KP-1")
	out := h.do(Bridge{Now: h.now, Online: true, Retained: true})
	check, ok := find[HealthCheck](out)
	if !ok || h.n.Status().Bridge || len(all[CarriesCode](out)) != 0 {
		t.Fatalf("asked, not believed: %+v", out)
	}
	last := check
	for i := range 2 {
		again, ok := find[HealthCheck](h.advance(3 * time.Second))
		if !ok || again.Txn == last.Txn {
			t.Fatalf("attempt %d: %+v", i+2, again)
		}
		last = again
	}
	h.advance(3 * time.Second)
	if h.n.Status().Bridge || !h.n.NextWake().After(h.now.Add(time.Hour)) {
		t.Fatal("no answer: offline, and no more requests")
	}
	if _, ok := find[HealthCheck](h.advance(time.Minute)); ok {
		t.Fatal("given up")
	}
	// A late answer counts; an answer to another request does not.
	if _, ok := find[Ignored](h.do(Health{Now: h.now, Txn: check.Txn})); !ok || h.n.Status().Bridge {
		t.Fatal("an older request's answer")
	}
	if out := h.do(Health{Now: h.now, Txn: last.Txn}); !h.n.Status().Bridge || len(all[CarriesCode](out)) != 1 {
		t.Fatalf("Zigbee2MQTT answers: online, and told the keypad again: %+v", out)
	}
	// Restarted, the adapter checks again; Zigbee2MQTT's live word is its own.
	again := h.restarted(h.now)
	if again.n.Status().Bridge {
		t.Fatal("the state saved is no evidence")
	}
	if out := again.do(Bridge{Now: again.now, Online: true}); !again.n.Status().Bridge || len(all[HealthCheck](out)) != 0 {
		t.Fatalf("a live online: %+v", out)
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
