package wire

import (
	"encoding/json"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/health"
	"safehome/internal/schematest"
)

var t0 = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

func event(dev, capability, value string) contract.DeviceEventData {
	return contract.DeviceEventData{DeviceID: dev, Capability: capability, Value: json.RawMessage(value)}
}

func TestInput(t *testing.T) {
	cases := []struct {
		name string
		d    contract.DeviceEventData
		want health.Input
		ok   bool
	}{
		{"mains lost", event("dev_hub", contract.CapMains, `"lost"`), health.Mains{Now: t0, Lost: true}, true},
		{"mains present", event("dev_hub", contract.CapMains, `"present"`), health.Mains{Now: t0}, true},
		{"battery", event("dev_door", contract.CapBattery, `37`), health.Battery{Now: t0, DeviceID: "dev_door", Percent: 37}, true},
		{"battery as text", event("dev_door", contract.CapBattery, `"full"`), nil, false},
		{"offline", event("dev_door", contract.CapAvailability, `"offline"`), health.Availability{Now: t0, DeviceID: "dev_door"}, true},
		{"online", event("dev_door", contract.CapAvailability, `"online"`), health.Availability{Now: t0, DeviceID: "dev_door", Online: true}, true},
		// Not supervision: they must never reach the monitor.
		{"contact", event("dev_door", contract.CapContact, `"open"`), nil, false},
		{"link", event("dev_door", contract.CapLink, `60`), nil, false},
		{"smoke", event("dev_smoke", contract.CapSmoke, `"alarm"`), nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Input(c.d, t0)
			if ok != c.ok || got != c.want {
				t.Fatalf("got %#v %v, want %#v %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestTranslateRefusesUnknownThings(t *testing.T) {
	for name, a := range map[string]health.Action{
		"kind":    health.Alert{Kind: "ufo", Urgency: health.UrgencyLow},
		"urgency": health.Alert{Kind: health.AlertBatteryLow, Urgency: "meh"},
	} {
		if _, err := Translate([]health.Action{a}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	type alien struct{ health.Action }
	if _, err := Translate([]health.Action{alien{}}); err == nil {
		t.Error("an unknown action must be an error")
	}
}

// A day of supervision, end to end: the events of the contract go in, and every
// alert that comes out is checked against the JSON Schema, envelope included.
func TestEverythingTheMonitorPublishesMatchesTheSchemas(t *testing.T) {
	cfg := health.DefaultConfig()
	cfg.HubDeviceID = "dev_hub"
	ids := contract.NewIDGenerator()
	m, err := health.New(cfg, ids.New, t0)
	if err != nil {
		t.Fatal(err)
	}

	now := t0
	var all []health.Action
	feed := func(d contract.DeviceEventData) {
		if err := d.Validate(); err != nil {
			t.Fatalf("test event is not valid: %v", err)
		}
		in, ok := Input(d, now)
		if !ok {
			t.Fatalf("%s not understood", d.Capability)
		}
		all = append(all, m.Handle(in)...)
	}
	feed(event("dev_hub", contract.CapBattery, `100`))
	feed(event("dev_hub", contract.CapMains, `"lost"`))
	feed(event("dev_hub", contract.CapBattery, `38`))
	feed(event("dev_hub", contract.CapBattery, `9`))
	feed(event("dev_hub", contract.CapMains, `"present"`))
	feed(event("dev_door", contract.CapAvailability, `"offline"`))
	feed(event("dev_motion", contract.CapBattery, `7`))
	now = now.Add(13 * time.Hour) // next morning
	all = append(all, m.Handle(health.Tick{Now: now})...)

	out, err := Translate(all)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, al := range out {
		seen[al.Kind]++
		schematest.Validate(t, "alert-request.json", al)
		env, err := contract.NewEnvelope("tg-lom-0001", "health", 1, ids.New(), time.Now(), contract.TypeAlertRequest, al)
		if err != nil {
			t.Fatalf("invalid envelope: %v", err)
		}
		schematest.Validate(t, "envelope.json", env)
	}
	want := map[string]int{
		contract.AlertBatteryLowHub: 2, // 40 % and 10 %
		contract.AlertMainsRestored: 1,
		contract.AlertDeviceOffline: 1,
		contract.AlertBatteryLow:    1,
	}
	for kind, n := range want {
		if seen[kind] != n {
			t.Errorf("%s: %d alerts, want %d", kind, seen[kind], n)
		}
	}
	if len(out) != 5 {
		t.Errorf("%d alerts, want 5: %+v", len(out), out)
	}
}

func TestGSMBackupIsUsableWithOneGoodLine(t *testing.T) {
	seen := t0
	good := contract.GSMLine{ID: "a", Responding: true, Registered: true, Signal: 60, Credit: "ok"}
	for name, c := range map[string]struct {
		lines []contract.GSMLine
		want  bool
	}{
		"no line":       {nil, false},
		"one good":      {[]contract.GSMLine{good}, true},
		"silent modem":  {[]contract.GSMLine{{ID: "a", Registered: true}}, false},
		"no network":    {[]contract.GSMLine{{ID: "a", Responding: true}}, false},
		"no credit":     {[]contract.GSMLine{{ID: "a", Responding: true, Registered: true, Credit: "empty"}}, false},
		"one of two ok": {[]contract.GSMLine{{ID: "a"}, good}, true},
	} {
		g := GSM(contract.GSMStatusData{Lines: c.lines}, seen)
		if g.Usable != c.want || !g.Seen.Equal(seen) {
			t.Errorf("%s: %+v", name, g)
		}
	}
}
