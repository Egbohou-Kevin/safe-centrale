package wire

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
	"safehome/internal/schematest"
)

var t0 = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestSignal(t *testing.T) {
	cases := []struct {
		name       string
		capability string
		value      string
		wantKind   engine.SensorKind
		wantActive bool
		wantOK     bool
	}{
		{"door open", contract.CapContact, `"open"`, engine.SensorContact, true, true},
		{"door closed", contract.CapContact, `"closed"`, engine.SensorContact, false, true},
		{"motion", contract.CapMotion, `"detected"`, engine.SensorMotion, true, true},
		{"motion cleared", contract.CapMotion, `"clear"`, engine.SensorMotion, false, true},
		{"smoke", contract.CapSmoke, `"alarm"`, engine.SensorSmoke, true, true},
		{"vibration", contract.CapVibration, `"detected"`, engine.SensorVibration, true, true},
		{"glass break", contract.CapGlassBreak, `"detected"`, engine.SensorGlassBreak, true, true},
		{"panic", contract.CapPanic, `"pressed"`, engine.SensorPanic, true, true},
		{"tamper", contract.CapTamper, `"triggered"`, engine.SensorTamper, true, true},
		{"tamper cleared", contract.CapTamper, `"clear"`, engine.SensorTamper, false, true},
		// Not zone signals: they must never reach the engine.
		{"battery", contract.CapBattery, `82`, "", false, false},
		{"link", contract.CapLink, `60`, "", false, false},
		{"keypad", contract.CapKeypad, `"arm"`, "", false, false},
		{"siren state", contract.CapSiren, `"on"`, "", false, false},
		{"mains", contract.CapMains, `"lost"`, "", false, false},
		{"availability", contract.CapAvailability, `"offline"`, "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := contract.DeviceEventData{DeviceID: "dev_1", Capability: c.capability, Value: raw(c.value)}
			sig, ok := Signal(d, t0)
			if ok != c.wantOK {
				t.Fatalf("ok=%v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if sig.Sensor != c.wantKind || sig.Active != c.wantActive || sig.DeviceID != "dev_1" || !sig.Now.Equal(t0) {
				t.Fatalf("signal %+v", sig)
			}
		})
	}
}

func TestCommand(t *testing.T) {
	exp := t0.Add(time.Minute)
	d := contract.AlarmCommandData{
		Command: contract.CmdDisarm,
		Args:    contract.CommandArgs{Code: "482915", Force: true, Devices: []string{"dev_1"}},
		Origin:  contract.OriginApp,
		User:    "u_42",
		Exp:     contract.TSPtr(exp),
	}
	auth := engine.Auth{Checked: true, OK: true, User: "u_42"}
	c := Command(d, "msg-1", t0, auth)

	want := engine.Command{
		Now: t0, ID: "msg-1", Name: engine.CmdDisarm, Origin: engine.OriginApp, User: "u_42",
		Force: true, Devices: []string{"dev_1"}, Exp: exp, Auth: auth,
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("command %+v, want %+v", c, want)
	}

	// Every contract command has an engine counterpart.
	for _, name := range []string{contract.CmdArm, contract.CmdArmNow, contract.CmdDisarm,
		contract.CmdSilence, contract.CmdBypass, contract.CmdPostpone} {
		got := Command(contract.AlarmCommandData{Command: name, Origin: contract.OriginKeypad}, "m", t0, engine.Auth{})
		if got.Name == engine.CommandName(name) {
			t.Errorf("%s was passed through instead of being translated", name)
		}
	}

	// An unknown command is passed through, and the engine refuses it.
	unknown := Command(contract.AlarmCommandData{Command: "alarm.explode", Origin: contract.OriginApp}, "m", t0, engine.Auth{})
	if unknown.Name != "alarm.explode" || !unknown.Exp.IsZero() {
		t.Fatalf("unknown command: %+v", unknown)
	}
}

func TestStateData(t *testing.T) {
	ends := t0.Add(2 * time.Minute)
	got, err := StateData(engine.State{Mode: engine.ModeArming, Since: t0, EndsAt: ends})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != contract.StateArming || got.EndsAt == nil || !got.EndsAt.Equal(ends) || got.LockedUntil != nil {
		t.Fatalf("%+v", got)
	}
	schematest.Validate(t, "alarm-state.json", got)

	locked, err := StateData(engine.State{Mode: engine.ModeTriggered, Since: t0, Cycle: 2, Of: 3, LockedUntil: ends})
	if err != nil {
		t.Fatal(err)
	}
	if locked.Cycle != 2 || locked.Of != 3 || locked.LockedUntil == nil {
		t.Fatalf("%+v", locked)
	}
	schematest.Validate(t, "alarm-state.json", locked)

	if _, err := StateData(engine.State{Mode: "asleep"}); err == nil {
		t.Fatal("an unknown mode must be an error")
	}
}

func TestTranslateRefusesUnknownThings(t *testing.T) {
	type alien struct{ engine.Event }
	for name, a := range map[string]engine.Action{
		"event":   engine.Emit{Event: alien{}},
		"status":  engine.Reply{Status: "maybe"},
		"reason":  engine.Reply{Status: engine.StatusRefused, Reason: "because"},
		"cleared": engine.Emit{Event: engine.Cleared{By: "nobody"}},
	} {
		if _, err := Translate([]engine.Action{a}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// The engine's alerts go to the escalation, not to the wire.
func TestTranslateLeavesTheAlertsOut(t *testing.T) {
	out, err := Translate([]engine.Action{engine.Alert{Kind: engine.AlertFire, Urgency: engine.UrgencyCritical}})
	if err != nil || !reflect.DeepEqual(out, Out{}) {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestTranslateSiren(t *testing.T) {
	out, err := Translate([]engine.Action{
		engine.SirenOn{Duration: 5 * time.Minute}, engine.SirenOff{},
		// What is left of a cycle, from a tick a little late: rounded up, never zero.
		engine.SirenOn{Duration: 4*time.Minute + 59*time.Second + 999*time.Millisecond},
		engine.SirenOn{Duration: 200 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []SirenCommand{
		{Command: contract.CmdSirenOn, DurationS: 300}, {Command: contract.CmdSirenOff},
		{Command: contract.CmdSirenOn, DurationS: 300}, {Command: contract.CmdSirenOn, DurationS: 1},
	}
	if !reflect.DeepEqual(out.Siren, want) {
		t.Fatalf("%+v", out.Siren)
	}
}

// The installer's tools check a device's capabilities against engine.Accepts by
// name: each sensor kind must be spelled like its capability.
func TestSensorKindsAreSpelledLikeTheCapabilities(t *testing.T) {
	for capability, kind := range sensors {
		if string(kind) != capability {
			t.Errorf("capability %q is sensor %q", capability, kind)
		}
	}
}

func TestForget(t *testing.T) {
	got := Forget(contract.RegistryDeviceRemovedData{Ref: "m-1", DeviceID: "dev_door"}, t0)
	if got != (engine.Forget{Now: t0, DeviceID: "dev_door"}) {
		t.Fatalf("%+v", got)
	}
}

// The two directions together: a real scenario through the engine, with every
// message it produces checked against the JSON Schemas, envelope included.
func TestEverythingTheEnginePublishesMatchesTheSchemas(t *testing.T) {
	cfg := engine.DefaultConfig()
	cfg.Zones = map[string]engine.Zone{
		"dev_door":   {Name: "entrance", Kind: engine.ZoneEntry},
		"dev_back":   {Name: "back", Kind: engine.ZoneInstant},
		"dev_window": {Name: "window", Kind: engine.ZoneInstant},
		"dev_smoke":  {Name: "smoke", Kind: engine.ZoneFire},
		"dev_panic":  {Name: "counter", Kind: engine.ZonePanic},
	}
	ids := contract.NewIDGenerator()
	e, err := engine.New(cfg, ids.New, t0)
	if err != nil {
		t.Fatal(err)
	}

	now := t0
	var all []engine.Action
	do := func(in engine.Input) { all = append(all, e.Handle(in)...) }
	tick := func(d time.Duration) { now = now.Add(d); do(engine.Tick{Now: now}) }
	signal := func(dev string, k engine.SensorKind, active bool) {
		do(engine.Signal{Now: now, DeviceID: dev, Sensor: k, Active: active})
	}
	n := 0
	cmd := func(name engine.CommandName, mutate ...func(*engine.Command)) {
		n++
		c := engine.Command{Now: now, ID: fmt.Sprintf("m-%d", n), Name: name, Origin: engine.OriginKeypad}
		for _, m := range mutate {
			m(&c)
		}
		do(c)
	}
	force := func(c *engine.Command) { c.Force = true; c.User = "u_42" }
	bad := func(c *engine.Command) { c.Auth = engine.Auth{Checked: true} }
	good := func(c *engine.Command) { c.Auth = engine.Auth{Checked: true, OK: true, User: "u_42"} }

	signal("dev_back", engine.SensorContact, true)
	cmd(engine.CmdArm, force) // arming, forced
	tick(2 * time.Minute)     // armed
	signal("dev_back", engine.SensorContact, false)
	signal("dev_door", engine.SensorContact, true) // entry delay
	cmd(engine.CmdDisarm, bad)                     // auth_failed
	cmd(engine.CmdDisarm, good)                    // disarmed
	signal("dev_door", engine.SensorContact, false)
	cmd(engine.CmdArm)
	tick(2 * time.Minute)
	signal("dev_window", engine.SensorGlassBreak, true) // triggered, siren cycle, alert
	tick(7 * time.Minute)                               // siren cycle 2
	cmd(engine.CmdDisarm, good)                         // cleared, disarmed
	cmd(engine.CmdArm)
	tick(2 * time.Minute)
	signal("dev_window", engine.SensorGlassBreak, true)
	for i := 0; i < 5; i++ {
		tick(7 * time.Minute) // cleared by timeout
	}
	signal("dev_smoke", engine.SensorSmoke, true)
	signal("dev_panic", engine.SensorPanic, true)
	cmd(engine.CmdArmNow) // refused: reply with a reason

	out, err := Translate(all)
	if err != nil {
		t.Fatal(err)
	}

	site := "tg-lom-0001"
	envelope := func(typ string, data any) {
		env, err := contract.NewEnvelope(site, "engine", 1, ids.New(), time.Now(), typ, data)
		if err != nil {
			t.Errorf("%s: invalid envelope: %v", typ, err)
			return
		}
		schematest.Validate(t, "envelope.json", env)
	}

	schemaFor := map[string]string{
		contract.TypeAlarmArming:     "alarm-arming.json",
		contract.TypeAlarmArmed:      "alarm-armed.json",
		contract.TypeAlarmDisarmed:   "alarm-disarmed.json",
		contract.TypeAlarmEntryDelay: "alarm-entry-delay.json",
		contract.TypeAlarmTriggered:  "alarm-triggered.json",
		contract.TypeAlarmSirenCycle: "alarm-siren-cycle.json",
		contract.TypeAlarmCleared:    "alarm-cleared.json",
		contract.TypeAlarmAuthFailed: "alarm-auth-failed.json",
	}
	seen := map[string]bool{}
	for _, ev := range out.Events {
		file, ok := schemaFor[ev.Type]
		if !ok {
			t.Fatalf("no schema known for %s", ev.Type)
		}
		seen[ev.Type] = true
		schematest.Validate(t, file, ev.Data)
		envelope(ev.Type, ev.Data)
	}
	for _, ack := range out.Acks {
		seen[contract.TypeAlarmAck] = true
		schematest.Validate(t, "alarm-ack.json", ack)
		envelope(contract.TypeAlarmAck, ack)
	}

	for typ := range schemaFor {
		if !seen[typ] {
			t.Errorf("the scenario never produced %s: it checks nothing for that type", typ)
		}
	}
	if !seen[contract.TypeAlarmAck] {
		t.Errorf("the scenario never produced %s", contract.TypeAlarmAck)
	}
	if len(out.Siren) == 0 {
		t.Error("the scenario never moved the siren")
	}

	// The retained state follows its schema too.
	state, err := StateData(e.State(now))
	if err != nil {
		t.Fatal(err)
	}
	schematest.Validate(t, "alarm-state.json", state)
	envelope(contract.TypeAlarmState, state)
}

// The automatic arming scenario: notice, postponement, arming with an open zone
// (so an alert naming the device), and the retained state announcing the next arming.
func TestScheduleMessagesMatchTheSchemas(t *testing.T) {
	cfg := engine.DefaultConfig()
	cfg.Zones = map[string]engine.Zone{"dev_back": {Name: "back", Kind: engine.ZoneInstant}}
	cfg.Closing = map[time.Weekday]engine.TimeOfDay{}
	for d := time.Sunday; d <= time.Saturday; d++ {
		cfg.Closing[d] = engine.TimeOfDay{Hour: 22}
	}
	ids := contract.NewIDGenerator()
	e, err := engine.New(cfg, ids.New, t0)
	if err != nil {
		t.Fatal(err)
	}

	state, err := StateData(e.State(t0))
	if err != nil {
		t.Fatal(err)
	}
	if state.NextScheduled == nil || !state.NextScheduled.Equal(t0.Add(time.Hour)) {
		t.Fatalf("the retained state must announce the next arming: %+v", state)
	}
	schematest.Validate(t, "alarm-state.json", state)

	now := t0
	var all []engine.Action
	step := func(d time.Duration, in func(time.Time) engine.Input) {
		now = now.Add(d)
		all = append(all, e.Handle(in(now))...)
	}
	tick := func(now time.Time) engine.Input { return engine.Tick{Now: now} }
	step(0, func(n time.Time) engine.Input {
		return engine.Signal{Now: n, DeviceID: "dev_back", Sensor: engine.SensorContact, Active: true}
	})
	step(0, func(n time.Time) engine.Input {
		return engine.Command{Now: n, ID: "m-1", Name: engine.CmdPostpone, Origin: engine.OriginApp, User: "u_42"}
	})
	step(0, func(n time.Time) engine.Input {
		return engine.Command{Now: n, ID: "m-2", Name: engine.CmdPostpone, Origin: engine.OriginApp}
	})
	step(0, func(n time.Time) engine.Input {
		return engine.Command{Now: n, ID: "m-3", Name: engine.CmdPostpone, Origin: engine.OriginApp}
	})
	step(0, func(n time.Time) engine.Input {
		return engine.Command{Now: n, ID: "m-4", Name: engine.CmdPostpone, Origin: engine.OriginApp} // refused: limit
	})
	step(2*time.Hour+20*time.Minute, tick) // 23:20: notice for 23:30
	step(10*time.Minute, tick)             // 23:30: arming starts with the back door open

	out, err := Translate(all)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		contract.TypeAlarmArmingScheduled: "alarm-arming-scheduled.json",
		contract.TypeAlarmPostponed:       "alarm-postponed.json",
		contract.TypeAlarmArming:          "alarm-arming.json",
	}
	seen := map[string]bool{}
	for _, ev := range out.Events {
		file, ok := want[ev.Type]
		if !ok {
			t.Fatalf("unexpected event %s", ev.Type)
		}
		seen[ev.Type] = true
		schematest.Validate(t, file, ev.Data)
	}
	for typ := range want {
		if !seen[typ] {
			t.Errorf("the scenario never produced %s", typ)
		}
	}

	var limit bool
	for _, ack := range out.Acks {
		schematest.Validate(t, "alarm-ack.json", ack)
		if ack.Reason == contract.ReasonPostponeLimit {
			limit = true
		}
	}
	if !limit {
		t.Error("the fourth postponement must be refused with postpone_limit")
	}

	// The alert itself goes to the escalation, whose wire checks its own messages.
	var alerts []engine.Alert
	for _, a := range all {
		if al, ok := a.(engine.Alert); ok {
			alerts = append(alerts, al)
		}
	}
	if len(alerts) != 1 || alerts[0].Kind != engine.AlertArmingBypassed || !reflect.DeepEqual(alerts[0].Devices, []string{"dev_back"}) {
		t.Fatalf("alerts: %+v", alerts)
	}
}

func TestSirenCommandData(t *testing.T) {
	exp := t0.Add(30 * time.Second)
	on := SirenCommand{Command: contract.CmdSirenOn, DurationS: 300}.Data("dev_s3x9", exp)
	if on.DeviceID != "dev_s3x9" || on.Command != contract.CmdSirenOn || on.Args.DurationS != 300 || on.Exp == nil || !on.Exp.Equal(exp) {
		t.Fatalf("%+v", on)
	}
	if err := on.Validate(); err != nil {
		t.Fatal(err)
	}
	schematest.Validate(t, "device-command.json", on)

	off := SirenCommand{Command: contract.CmdSirenOff}.Data("dev_s3x9", exp)
	if err := off.Validate(); err != nil {
		t.Fatal(err)
	}
	schematest.Validate(t, "device-command.json", off)
}
