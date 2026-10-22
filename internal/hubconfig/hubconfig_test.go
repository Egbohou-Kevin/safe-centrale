package hubconfig

import (
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"safehome/internal/configfile"
	"safehome/internal/engine"
	"safehome/internal/escalation"
	escwire "safehome/internal/escalation/wire"
	"safehome/internal/health"
	"safehome/internal/install"
)

// parse reads and checks a configuration given as bytes.
func parse(data []byte) (Result, error) {
	var f file
	if err := configfile.DecodeBytes(data, &f); err != nil {
		return Result{}, err
	}
	return build(f)
}

// minimal is the smallest valid file: everything else takes the documented defaults.
const minimal = `{
  "site": "tg-lom-0001",
  "state_dir": "/tmp/safehome",
  "hub_device_id": "dev_hubpower",
  "siren_ids": ["dev_siren1"]
}`

// base is minimal without its closing brace, to add keys to.
var base = strings.TrimSuffix(strings.TrimSpace(minimal), "}")

func with(t *testing.T, extra string) Result {
	t.Helper()
	body := strings.TrimSuffix(strings.TrimSpace(minimal), "}") + "," + extra + "}"
	r, err := parse([]byte(body))
	if err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	return r
}

func TestTheExampleFileIsValid(t *testing.T) {
	r, err := Load(filepath.Join("..", "..", "config", "hub.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Hub.Site != "tg-lom-0001" || len(r.Hub.Engine.Zones) != 7 || r.StateDir != "/var/lib/safehome" {
		t.Fatalf("%+v", r)
	}
	// The example repeats the documented defaults: it must say what the code does.
	def := engine.DefaultConfig()
	got := r.Hub.Engine
	if got.ExitDelay != def.ExitDelay || got.EntryDelay != def.EntryDelay || got.SirenCycle != def.SirenCycle ||
		got.SirenPause != def.SirenPause || got.SirenCycles != def.SirenCycles || got.LockoutAttempts != def.LockoutAttempts ||
		got.LockoutDuration != def.LockoutDuration || got.PanicSiren != def.PanicSiren || got.AutoArmNotice != def.AutoArmNotice ||
		got.PostponeStep != def.PostponeStep || got.PostponeMax != def.PostponeMax || got.AutoArmLimit != def.AutoArmLimit {
		t.Fatalf("the example differs from the defaults: %+v", got)
	}
	hd := health.DefaultConfig()
	hc := r.Hub.Health
	if hc.HubBatteryWarn != hd.HubBatteryWarn || hc.HubBatteryCritical != hd.HubBatteryCritical || hc.HubBatteryRearm != hd.HubBatteryRearm ||
		hc.DeviceBatteryLow != hd.DeviceBatteryLow || hc.DeviceBatteryRearm != hd.DeviceBatteryRearm || hc.SummaryAt.Hour != 9 {
		t.Fatalf("the example differs from the health defaults: %+v", hc)
	}
	if r.Hub.Access.Iterations != 200_000 {
		t.Fatalf("%d", r.Hub.Access.Iterations)
	}
	ed := escalation.DefaultConfig("", nil)
	ec := r.Hub.Escalation
	if ec.InternetConfirm != ed.InternetConfirm || ec.GSMDelay != ed.GSMDelay || ec.MaxPerHour != ed.MaxPerHour {
		t.Fatalf("the example differs from the escalation defaults: %+v", ec)
	}
	if ec.SiteName != "Boutique Lome" || len(ec.Contacts) != 4 || ec.DeviceNames["dev_back"] != "Porte arriere" {
		t.Fatalf("%+v", ec)
	}
	// The example writes every plan out, and they are the defaults: the file is where
	// an installer reads what happens for each alert.
	if !reflect.DeepEqual(ec.Plans, escalation.DefaultPlans()) {
		t.Fatalf("the example's plans differ from the defaults:\n%+v\n%+v", ec.Plans, escalation.DefaultPlans())
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "hub.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		Escalation struct {
			Plans map[string]json.RawMessage `json:"plans"`
		} `json:"escalation"`
	}
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(written.Escalation.Plans)); !reflect.DeepEqual(got, escwire.KindNames()) {
		t.Fatalf("the example writes the plans of %v, want all of %v", got, escwire.KindNames())
	}
}

func TestAPlanReplacesOnlyItsKind(t *testing.T) {
	r, err := parse([]byte(base + `,
  "escalation": { "plans": {
    "intrusion": { "waves": [ { "to": ["owner"] }, { "after": "3m", "to": ["everyone"], "sms_now": true } ], "repeat": { "every": "5m", "times": 6 } },
    "battery_low_hub": { "waves": [ { "to": ["owner", "guard"] } ], "internet_only": true, "gsm_after": "30m" }
  } }
}`))
	if err != nil {
		t.Fatal(err)
	}
	plans := r.Hub.Escalation.Plans
	want := escalation.Plan{
		Waves: []escalation.Wave{
			{To: []escalation.Audience{escalation.AudienceOwner}},
			{After: 3 * time.Minute, To: []escalation.Audience{escalation.AudienceEveryone}, SMSNow: true},
		},
		Repeat: escalation.Repeat{Every: 5 * time.Minute, Times: 6},
	}
	if !reflect.DeepEqual(plans[escalation.KindIntrusion], want) {
		t.Fatalf("intrusion: %+v", plans[escalation.KindIntrusion])
	}
	hb := plans[escalation.KindHubBatteryLow]
	if !hb.InternetOnly || hb.GSMAfter != 30*time.Minute || len(hb.Waves[0].To) != 2 {
		t.Fatalf("battery_low_hub: %+v", hb)
	}
	defaults := escalation.DefaultPlans()
	if !reflect.DeepEqual(plans[escalation.KindFire], defaults[escalation.KindFire]) || len(plans) != len(defaults) {
		t.Fatal("the kinds left out lost their default plan")
	}
}

func TestBadPlansAreRefusedWithTheirPlace(t *testing.T) {
	for want, plans := range map[string]string{
		`unknown alert kind "flood"`:                                        `"flood": { "waves": [ { "to": ["owner"] } ] }`,
		`escalation.plans.fire.waves[2].after`:                              `"fire": { "waves": [ { "to": ["owner"] }, { "after": "soon", "to": ["owner"] } ] }`,
		`escalation.plans.fire: wave 2 starts before`:                       `"fire": { "waves": [ { "after": "5m", "to": ["owner"] }, { "after": "1m", "to": ["owner"] } ] }`,
		`escalation.plans.battery_low_hub: wave 1: unknown audience "boss"`: `"battery_low_hub": { "waves": [ { "to": ["boss"] } ] }`,
		`escalation.plans.panic: at least one wave`:                         `"panic": { "waves": [] }`,
		`escalation.plans.panic: wave 1 tells nobody`:                       `"panic": { "waves": [ { "to": [] } ] }`,
		`escalation.plans.tamper.repeat.every`:                              `"tamper": { "waves": [ { "to": ["owner"] } ], "repeat": { "every": "often", "times": 2 } }`,
		`escalation.plans.tamper: reminders`:                                `"tamper": { "waves": [ { "to": ["owner"] } ], "repeat": { "times": 2 } }`,
		`escalation.plans.tamper.gsm_after`:                                 `"tamper": { "waves": [ { "to": ["owner"] } ], "internet_only": true, "gsm_after": "later" }`,
		`escalation.plans.tamper: GSMAfter`:                                 `"tamper": { "waves": [ { "to": ["owner"] } ], "gsm_after": "1h" }`,
		`unknown field "delay"`:                                             `"tamper": { "waves": [ { "to": ["owner"], "delay": "1m" } ] }`,
	} {
		_, err := parse([]byte(base + `, "escalation": { "plans": { ` + plans + ` } } }`))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error with %q", plans, err, want)
		}
	}
}

func TestContactsAndEscalation(t *testing.T) {
	r := with(t, `"site_name": "Chez Ama", "timezone": "Africa/Lome",
		"zones": {"dev_door": {"name": "Entree", "kind": "entry"}},
		"contacts": [
			{"id": "u1", "name": "Ama", "role": "owner", "channels": ["app", "sms"]},
			{"id": "u2", "role": "guard", "channels": ["sms", "call"]}],
		"escalation": {"internet_confirm": "20s", "gsm_delay": "2m", "max_per_hour": 5}`)
	e := r.Hub.Escalation
	if e.SiteName != "Chez Ama" || len(e.Contacts) != 2 || e.Contacts[1].Role != escalation.RoleGuard ||
		len(e.Contacts[0].Channels) != 2 || e.Contacts[0].Channels[1] != escalation.ChannelSMS {
		t.Fatalf("%+v", e)
	}
	if e.InternetConfirm != 20*time.Second || e.GSMDelay != 2*time.Minute || e.MaxPerHour != 5 ||
		e.Location == nil || e.DeviceNames["dev_door"] != "Entree" {
		t.Fatalf("%+v", e)
	}
	// Without a site name, the texts use the site identifier.
	if r := with(t, `"contacts": []`); r.Hub.Escalation.SiteName != "tg-lom-0001" {
		t.Fatalf("%q", r.Hub.Escalation.SiteName)
	}
}

func TestDefaults(t *testing.T) {
	r, err := parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if r.BrokerURL != "mqtt://127.0.0.1:1883" || r.ClientID != "safehome-hub-tg-lom-0001" || r.LogLevel != slog.LevelInfo {
		t.Fatalf("%+v", r)
	}
	if r.Hub.Engine.ExitDelay != 2*time.Minute || len(r.Hub.Engine.Closing) != 0 || r.Hub.Engine.Location != nil {
		t.Fatalf("%+v", r.Hub.Engine)
	}
	if r.Hub.HubDeviceID != "dev_hubpower" || len(r.Hub.SirenIDs) != 1 {
		t.Fatalf("%+v", r.Hub)
	}
}

func TestOverrides(t *testing.T) {
	r := with(t, `"alarm": {"exit_delay": "90s", "entry_delay": "1m", "siren_cycles": 2, "panic_siren": true, "postpone_max": 0},
		"health": {"hub_battery_warn": 50, "summary_at": "07:30", "gsm_degraded_after": "15m", "gsm_silent_after": "5m",
			"zigbee_silent_after": "4m", "zigbee_down_after": "2m"},
		"access": {"iterations": 50000},
		"heartbeat_every": "30s", "boot_window": "0s", "log_level": "debug",
		"broker_url": "mqtt://10.0.0.5:1884", "client_id": "hub-a", "versions": {"engine": "0.1.0"}`)
	e := r.Hub.Engine
	if e.ExitDelay != 90*time.Second || e.EntryDelay != time.Minute || e.SirenCycles != 2 || !e.PanicSiren || e.PostponeMax != 0 {
		t.Fatalf("%+v", e)
	}
	if r.Hub.Health.HubBatteryWarn != 50 || r.Hub.Health.SummaryAt != (health.TimeOfDay{Hour: 7, Minute: 30}) ||
		r.Hub.Health.GSMDegradedAfter != 15*time.Minute || r.Hub.Health.GSMSilentAfter != 5*time.Minute ||
		r.Hub.Health.ZigbeeSilentAfter != 4*time.Minute || r.Hub.Health.ZigbeeDownAfter != 2*time.Minute {
		t.Fatalf("%+v", r.Hub.Health)
	}
	if r.Hub.Access.Iterations != 50000 || r.Hub.HeartbeatEvery != 30*time.Second || r.Hub.BootWindow != 0 ||
		r.LogLevel != slog.LevelDebug || r.BrokerURL != "mqtt://10.0.0.5:1884" || r.ClientID != "hub-a" || r.Hub.Versions["engine"] != "0.1.0" {
		t.Fatalf("%+v", r)
	}
}

func TestClosingTimes(t *testing.T) {
	r := with(t, `"closing": {"daily": "20:00", "sat": "18:30", "sun": "off"}`)
	c := r.Hub.Engine.Closing
	if len(c) != 6 || c[time.Monday] != (engine.TimeOfDay{Hour: 20}) || c[time.Saturday] != (engine.TimeOfDay{Hour: 18, Minute: 30}) {
		t.Fatalf("%v", c)
	}
	if _, ok := c[time.Sunday]; ok {
		t.Fatal("off means no automatic arming that day")
	}
	only := with(t, `"closing": {"fri": "21:15"}`)
	if len(only.Hub.Engine.Closing) != 1 || only.Hub.Engine.Closing[time.Friday] != (engine.TimeOfDay{Hour: 21, Minute: 15}) {
		t.Fatalf("%v", only.Hub.Engine.Closing)
	}
	empty := with(t, `"closing": {}`)
	if len(empty.Hub.Engine.Closing) != 0 {
		t.Fatal("an empty table turns the feature off")
	}
}

func TestTimezone(t *testing.T) {
	r := with(t, `"timezone": "Africa/Lome", "closing": {"daily": "20:00"}`)
	if r.Hub.Engine.Location == nil || r.Hub.Health.Location == nil || r.Hub.Engine.Location.String() != "Africa/Lome" {
		t.Fatalf("%v", r.Hub.Engine.Location)
	}
	// Lome is on UTC: a closing time of 20:00 there is 20:00 UTC.
	e, err := engine.New(r.Hub.Engine, func() string { return "x" }, time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got := e.State(time.Time{}).NextScheduled; !got.Equal(time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC)) {
		t.Fatalf("%v", got)
	}
}

func TestZones(t *testing.T) {
	r := with(t, `"zones": {"dev_door": {"name": "Entree", "kind": "entry"}, "dev_smoke": {"name": "Fumee", "kind": "24h_fire"}}`)
	z := r.Hub.Engine.Zones
	if len(z) != 2 || z["dev_door"] != (engine.Zone{Name: "Entree", Kind: engine.ZoneEntry}) || z["dev_smoke"].Kind != engine.ZoneFire {
		t.Fatalf("%v", z)
	}
}

// A zone name goes into SMS, voice messages and the alarm events (device_name): a
// hand-edited file follows the installer's rule (install.ValidName), and the error
// names the zone.
func TestAZoneNameFollowsTheInstallersRule(t *testing.T) {
	for _, name := range []string{`Porte\nArriere`, `Porte\u202eArriere`, `Porte\u200bArriere`, `   `, strings.Repeat("a", install.MaxNameLength+1)} {
		body := base + `,"zones":{"dev_back":{"name":"` + name + `","kind":"entry"}}}`
		_, err := parse([]byte(body))
		if err == nil || !strings.Contains(err.Error(), `"dev_back"`) {
			t.Errorf("zone name %s: %v", name, err)
		}
	}
}

func TestMistakesAreRefused(t *testing.T) {
	bad := map[string]string{
		"not json":                   `site: tg`,
		"trailing data":              minimal + ` {}`,
		"unknown key":                `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"exit_dealy":"2m"}`,
		"unknown key in alarm":       `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"alarm":{"exit_dealy":"2m"}}`,
		"no state dir":               `{"site":"tg-lom-0001","hub_device_id":"h","siren_ids":["s"]}`,
		"no site":                    `{"state_dir":"/x","hub_device_id":"h","siren_ids":["s"]}`,
		"bad site":                   `{"site":"Lome","state_dir":"/x","hub_device_id":"h","siren_ids":["s"]}`,
		"no hub device":              `{"site":"tg-lom-0001","state_dir":"/x","siren_ids":["s"]}`,
		"no siren":                   `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h"}`,
		"bad duration":               `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"alarm":{"exit_delay":"2 minutes"}}`,
		"negative duration":          `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"alarm":{"exit_delay":"-2m"}}`,
		"duration as number":         `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"alarm":{"exit_delay":120}}`,
		"bad heartbeat":              `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"heartbeat_every":"soon"}`,
		"zero heartbeat":             `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"heartbeat_every":"0s"}`,
		"bad closing time":           `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"closing":{"daily":"8pm"}}`,
		"closing hour 24":            `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"closing":{"daily":"24:00"}}`,
		"bad closing day":            `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"closing":{"monday":"20:00"}}`,
		"bad gsm degraded after":     `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"health":{"gsm_degraded_after":"soon"}}`,
		"bad gsm silent after":       `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"health":{"gsm_silent_after":"3"}}`,
		"bad zigbee down after":      `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"health":{"zigbee_down_after":"x"}}`,
		"zero gsm silent after":      `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"health":{"gsm_silent_after":"0s"}}`,
		"bad summary time":           `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"health":{"summary_at":"9h"}}`,
		"bad zone kind":              `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"zones":{"d":{"name":"x","kind":"garage"}}}`,
		"zone without a name":        `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"zones":{"d":{"kind":"entry"}}}`,
		"bad timezone":               `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"timezone":"Mars/Olympus"}`,
		"bad log level":              `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"log_level":"loud"}`,
		"inverted battery levels":    `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"health":{"hub_battery_critical":50}}`,
		"too few hash iterations":    `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"access":{"iterations":100}}`,
		"contact with a bad role":    `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"contacts":[{"id":"a","role":"king","channels":["sms"]}]}`,
		"contact with no channel":    `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"contacts":[{"id":"a","role":"owner"}]}`,
		"contact with a bad channel": `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"contacts":[{"id":"a","role":"owner","channels":["pigeon"]}]}`,
		"no owner among contacts":    `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"contacts":[{"id":"a","role":"guard","channels":["sms"]}]}`,
		"unknown key in a contact":   `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"contacts":[{"id":"a","role":"owner","channels":["sms"],"phone":"+22890000000"}]}`,
		"bad internet confirm":       `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"escalation":{"internet_confirm":"soon"}}`,
		"bad gsm delay":              `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"escalation":{"gsm_delay":"x"}}`,
		"zero hourly limit":          `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"escalation":{"max_per_hour":0}}`,
		"zero siren cycles":          `{"site":"tg-lom-0001","state_dir":"/x","hub_device_id":"h","siren_ids":["s"],"alarm":{"siren_cycles":0}}`,
	}
	for name, body := range bad {
		if _, err := parse([]byte(body)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("a missing file is an error")
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"site": 12}`), 0o600)
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "bad.json") {
		t.Fatalf("the error must name the file: %v", err)
	}
}
