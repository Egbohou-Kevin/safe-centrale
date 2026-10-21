package hub

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/engine"
	"safehome/internal/inbox"
)

// --- tests -------------------------------------------------------------------------

func TestBootPublishesTheRetainedStateAndHealth(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()

	st := r.broker.On(contract.AlarmStateTopic(site))
	if len(st) == 0 || !st[0].Retain || st[0].QoS != 1 || st[0].Env.Src != "engine" {
		t.Fatalf("alarm state: %+v", st)
	}
	if d := dataOf[contract.AlarmStateData](t, st[0]); d.State != contract.StateDisarmed {
		t.Fatalf("%+v", d)
	}
	h := r.broker.On(contract.HealthTopic(site))
	if len(h) == 0 || !h[0].Retain || h[0].Env.Src != "health" {
		t.Fatalf("health: %+v", h)
	}
	if d := dataOf[contract.HealthData](t, h[0]); d.Mains != "" || d.HubBattery != nil || d.Links == nil {
		t.Fatalf("nothing is known yet: %+v", d)
	}
	r.validateAll()
}

func TestAnIntrusionFromStartToFinish(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.setCode("u_owner", "482915")

	r.order(contract.CmdArm, contract.CommandArgs{}, contract.OriginApp, "u_owner")
	r.mustBe(engine.ModeArming)
	r.clock.Advance(2 * time.Minute)
	drivertest.WaitFor(t, "armed", func() bool { return len(eventsOf(r, contract.TypeAlarmArmed)) == 1 })
	r.mustBe(engine.ModeArmed)

	r.device(devBack, contract.CapContact, "open") // perimeter door forced
	drivertest.WaitFor(t, "the siren order", func() bool { return len(r.broker.On(contract.DeviceCmdTopic(site, devSiren))) == 1 })
	r.settle()

	siren := r.broker.On(contract.DeviceCmdTopic(site, devSiren))[0]
	cmd := dataOf[contract.DeviceCommandData](t, siren)
	if cmd.Command != contract.CmdSirenOn || cmd.Args.DurationS != 300 || cmd.Exp == nil ||
		!cmd.Exp.Equal(r.clock.Now().Add(30*time.Second)) || siren.QoS != 1 || siren.Retain || siren.Env.Type != contract.TypeCommand {
		t.Fatalf("siren command: %+v", cmd)
	}
	if len(eventsOf(r, contract.TypeAlarmTriggered)) != 1 || len(eventsOf(r, contract.TypeAlarmSirenCycle)) != 1 {
		t.Fatalf("events: %d triggered, %d cycle", len(eventsOf(r, contract.TypeAlarmTriggered)), len(eventsOf(r, contract.TypeAlarmSirenCycle)))
	}
	alerts := eventsOf(r, contract.TypeAlertRequest)
	if len(alerts) != 1 {
		t.Fatalf("alerts: %d", len(alerts))
	}
	// The owner is the first person told, through the internet channel first.
	if a := dataOf[contract.AlertRequestData](t, alerts[0]); a.Kind != contract.AlertIntrusion || a.Urgency != contract.UrgencyHigh ||
		a.Incident == "" || a.Contact != "u_owner" || a.Channel != contract.ChannelApp || a.Wave != 1 ||
		!strings.Contains(a.Text, "intrusion") || alerts[0].Env.Src != "escalation" {
		t.Fatalf("%+v", a)
	}
	if st := r.broker.On(contract.AlarmStateTopic(site)); dataOf[contract.AlarmStateData](t, st[len(st)-1]).State != contract.StateTriggered {
		t.Fatal("the retained state must follow the alarm")
	}

	// The owner disarms from the app, code checked on the hub.
	r.order(contract.CmdDisarm, contract.CommandArgs{Code: "482915"}, contract.OriginApp, "u_owner")
	drivertest.WaitFor(t, "disarmed", func() bool { return len(eventsOf(r, contract.TypeAlarmDisarmed)) == 1 })
	r.mustBe(engine.ModeDisarmed)
	off := r.broker.On(contract.DeviceCmdTopic(site, devSiren))
	if len(off) != 2 || dataOf[contract.DeviceCommandData](t, off[1]).Command != contract.CmdSirenOff {
		t.Fatalf("siren orders: %d", len(off))
	}
	if d := dataOf[contract.DisarmedData](t, eventsOf(r, contract.TypeAlarmDisarmed)[0]); d.User != "u_owner" || d.Origin != contract.OriginApp {
		t.Fatalf("%+v", d)
	}
	r.settle()
	r.validateAll()
}

// The siren leaves first, then the engine's messages in its order. The alert goes
// through the escalation's own loop: its request follows the siren, a moment later.
func TestTheSirenLeavesFirstThenTheEventsAndTheAlert(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.order(contract.CmdArm, contract.CommandArgs{}, contract.OriginApp, "u")
	r.mustBe(engine.ModeArming)
	r.clock.Advance(2 * time.Minute)
	r.mustBe(engine.ModeArmed)
	before := len(r.broker.All())
	r.device(devBack, contract.CapContact, "open")
	r.settle()
	var engineOrder []string
	alerts := 0
	for i, m := range r.broker.All()[before:] {
		if m.Env.Type != contract.TypeAlertRequest {
			engineOrder = append(engineOrder, m.Env.Type)
			continue
		}
		if alerts++; i == 0 {
			t.Fatal("the alert request left before the siren")
		}
	}
	want := []string{contract.TypeCommand, contract.TypeAlarmTriggered, contract.TypeAlarmSirenCycle, contract.TypeAlarmState}
	if !slices.Equal(engineOrder, want) || alerts != 1 {
		t.Fatalf("order of departure: %v and %d alert requests, want %v and one", engineOrder, alerts, want)
	}
}

func TestKeypadDisarmWithTheRightCode(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.setCode("u_owner", "730264")
	r.keypad(contract.ValArm, "")
	r.mustBe(engine.ModeArming)
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)

	r.device(devDoor, contract.CapContact, "open") // entry delay starts
	r.mustBe(engine.ModeEntryDelay)
	r.keypad(contract.ValDisarm, "730264")
	drivertest.WaitFor(t, "disarmed", func() bool { return len(eventsOf(r, contract.TypeAlarmDisarmed)) == 1 })
	r.mustBe(engine.ModeDisarmed)
	d := dataOf[contract.DisarmedData](t, eventsOf(r, contract.TypeAlarmDisarmed)[0])
	if d.User != "u_owner" || d.Origin != contract.OriginKeypad {
		t.Fatalf("%+v", d)
	}
	r.settle()
	r.validateAll()
}

func TestWrongCodesLockTheKeypadAndAlertAndNeverLeak(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.setCode("u_owner", "482915")
	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)

	for _, bad := range []string{"111122", "222233", "333344"} {
		r.keypad(contract.ValDisarm, bad)
		r.settle()
	}
	drivertest.WaitFor(t, "three failures", func() bool { return len(eventsOf(r, contract.TypeAlarmAuthFailed)) == 3 })
	last := dataOf[contract.AuthFailedData](t, eventsOf(r, contract.TypeAlarmAuthFailed)[2])
	if last.Attempts != 3 || last.LockedUntil == nil {
		t.Fatalf("%+v", last)
	}
	// The escalation hears of the third failure after it is published: wait for it.
	drivertest.WaitFor(t, "the auth_failed alert", func() bool {
		var kinds []string
		for _, a := range eventsOf(r, contract.TypeAlertRequest) {
			kinds = append(kinds, dataOf[contract.AlertRequestData](t, a).Kind)
		}
		return contains(kinds, contract.AlertAuthFailed)
	})

	// Locked: even the right code is refused now.
	acks := len(r.broker.OfType(contract.TypeAlarmAck))
	r.keypad(contract.ValDisarm, "482915")
	r.waitAcks(acks + 1)
	r.mustBe(engine.ModeArmed)
	if got := lastAck(t, r); got.Reason != contract.ReasonLockedOut {
		t.Fatalf("%+v", got)
	}

	// Nothing that was published or logged contains any of the codes.
	all := string(joinPayloads(r.broker.All())) + r.logs.String()
	for _, code := range []string{"482915", "111122", "222233", "333344"} {
		if strings.Contains(all, code) {
			t.Errorf("the code %s leaks into the messages or the logs", code)
		}
	}
	r.validateAll()
}

func lastAck(t *testing.T, r *rig) contract.AlarmAckData {
	t.Helper()
	acks := r.broker.OfType(contract.TypeAlarmAck)
	if len(acks) == 0 {
		t.Fatal("no ack")
	}
	return dataOf[contract.AlarmAckData](t, acks[len(acks)-1])
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func joinPayloads(ms []published) []byte {
	var b []byte
	for _, m := range ms {
		b = append(b, m.Payload...)
	}
	return b
}

func TestADisarmWithoutACodeCountsAsAWrongOne(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)
	r.keypad(contract.ValDisarm, "")
	r.settle()
	r.mustBe(engine.ModeArmed)
	if len(eventsOf(r, contract.TypeAlarmAuthFailed)) != 1 {
		t.Fatalf("auth failures: %d", len(eventsOf(r, contract.TypeAlarmAuthFailed)))
	}
}

func TestCommandsAreAcknowledgedWithTheirOwnID(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	id := r.order(contract.CmdArm, contract.CommandArgs{}, contract.OriginApp, "u")
	r.settle()
	acks := r.broker.On(contract.AlarmAckTopic(site))
	if len(acks) != 1 {
		t.Fatalf("%d acks", len(acks))
	}
	if a := dataOf[contract.AlarmAckData](t, acks[0]); a.Ref != id || a.Status != contract.AckDone {
		t.Fatalf("%+v", a)
	}
	r.validateAll()
}

func TestADuplicateMessageIsHandledOnce(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	env, _ := contract.NewEnvelope(site, "test", 1, r.ids.New(), r.clock.Now(), contract.TypeAlarmCommand,
		contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp})
	b, _ := json.Marshal(env)
	for i := 0; i < 3; i++ { // QoS 1 may deliver more than once
		r.hub.HandleMessage(inbox.Message{Topic: contract.AlarmCmdTopic(site), Payload: b})
	}
	r.settle()
	if n := len(r.broker.On(contract.AlarmAckTopic(site))); n != 1 {
		t.Fatalf("%d acks for one order", n)
	}
	if n := len(eventsOf(r, contract.TypeAlarmArming)); n != 1 {
		t.Fatalf("%d arming events", n)
	}
}

func TestGarbageIsIgnoredAndNeverLogged(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	good := func() []byte {
		env, _ := contract.NewEnvelope(site, "test", 1, r.ids.New(), r.clock.Now(), contract.TypeAlarmCommand,
			contract.AlarmCommandData{Command: contract.CmdDisarm, Args: contract.CommandArgs{Code: "9999"}, Origin: contract.OriginApp})
		b, _ := json.Marshal(env)
		return b
	}()
	otherSite := bytes.Replace(good, []byte(site), []byte("tg-lom-0002"), 1)
	wrongType := bytes.Replace(good, []byte(`"type":"alarm.command"`), []byte(`"type":"device.event"`), 1)
	for name, m := range map[string]inbox.Message{
		"not json":              {Topic: contract.AlarmCmdTopic(site), Payload: []byte(`not json 9999`)},
		"empty":                 {Topic: contract.AlarmCmdTopic(site), Payload: nil},
		"another site":          {Topic: contract.AlarmCmdTopic(site), Payload: otherSite},
		"another site topic":    {Topic: contract.AlarmCmdTopic("tg-lom-0002"), Payload: good},
		"wrong type":            {Topic: contract.AlarmCmdTopic(site), Payload: wrongType},
		"foreign topic":         {Topic: "other/thing", Payload: good},
		"unknown topic":         {Topic: contract.Topic(site, "alarm", "explode"), Payload: good},
		"oversized":             {Topic: contract.AlarmCmdTopic(site), Payload: bytes.Repeat([]byte("x"), maxPayload+1)},
		"unknown command":       {Topic: contract.AlarmCmdTopic(site), Payload: mustEnv(t, r, contract.TypeAlarmCommand, map[string]any{"command": "", "args": map[string]any{}, "origin": "app"})},
		"origin sms":            {Topic: contract.AlarmCmdTopic(site), Payload: mustEnv(t, r, contract.TypeAlarmCommand, map[string]any{"command": "alarm.arm", "args": map[string]any{}, "origin": "sms"})},
		"device topic mismatch": {Topic: contract.DeviceEventTopic(site, "dev_a"), Payload: mustEnv(t, r, contract.TypeDeviceEvent, event("dev_b", contract.CapContact, "open"))},
		"invalid device value":  {Topic: contract.DeviceEventTopic(site, "dev_a"), Payload: mustEnv(t, r, contract.TypeDeviceEvent, event("dev_a", contract.CapContact, "ajar"))},
		"code on a door":        {Topic: contract.DeviceEventTopic(site, "dev_a"), Payload: mustEnv(t, r, contract.TypeDeviceEvent, map[string]any{"device_id": "dev_a", "capability": "contact", "value": "open", "code": "9999"})},
	} {
		before := len(r.broker.All())
		r.hub.HandleMessage(m)
		r.settle()
		if len(r.broker.All()) != before {
			t.Errorf("%s: the hub reacted", name)
		}
	}
	if strings.Contains(r.logs.String(), "9999") {
		t.Errorf("a payload leaked into the logs:\n%s", r.logs.String())
	}
	r.mustBe(engine.ModeDisarmed)
}

func mustEnv(t *testing.T, r *rig, typ string, data any) []byte {
	t.Helper()
	r.seq++
	env, err := contract.NewEnvelope(site, "test", uint64(r.seq), r.ids.New(), r.clock.Now(), typ, data)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	return b
}

func TestEntryDelayThenSirenWhenNobodyDisarms(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.device(devDoor, contract.CapContact, "open")
	r.mustBe(engine.ModeEntryDelay)
	drivertest.WaitFor(t, "entry delay event", func() bool { return len(eventsOf(r, contract.TypeAlarmEntryDelay)) == 1 })
	r.clock.Advance(45 * time.Second)
	drivertest.WaitFor(t, "the siren", func() bool { return len(r.broker.On(contract.DeviceCmdTopic(site, devSiren))) == 1 })
	r.mustBe(engine.ModeTriggered)
	r.settle()
	r.validateAll()
}

func TestAutomaticArmingRunsFromTheClock(t *testing.T) {
	r := newRig(t, nil, func(c *Config) {
		c.Engine.Closing = map[time.Weekday]engine.TimeOfDay{time.Saturday: {Hour: 22}}
	})
	r.start()
	r.clock.Advance(50 * time.Minute) // 21:50
	drivertest.WaitFor(t, "the notice", func() bool { return len(eventsOf(r, contract.TypeAlarmArmingScheduled)) == 1 })
	r.order(contract.CmdPostpone, contract.CommandArgs{}, contract.OriginApp, "u_owner")
	drivertest.WaitFor(t, "the postponement", func() bool { return len(eventsOf(r, contract.TypeAlarmPostponed)) == 1 })
	r.clock.Advance(40 * time.Minute) // 22:30
	drivertest.WaitFor(t, "the arming", func() bool { return len(eventsOf(r, contract.TypeAlarmArming)) == 1 })
	if d := dataOf[contract.ArmingData](t, eventsOf(r, contract.TypeAlarmArming)[0]); d.Origin != contract.OriginSchedule {
		t.Fatalf("%+v", d)
	}
	r.settle()
	st := r.broker.On(contract.AlarmStateTopic(site))
	if last := dataOf[contract.AlarmStateData](t, st[len(st)-1]); last.State != contract.StateArming {
		t.Fatalf("%+v", last)
	}
	r.validateAll()
}

func TestStateAnnouncesTheNextScheduledArming(t *testing.T) {
	r := newRig(t, nil, func(c *Config) {
		c.Engine.Closing = map[time.Weekday]engine.TimeOfDay{time.Saturday: {Hour: 22}}
	})
	r.start()
	st := r.broker.On(contract.AlarmStateTopic(site))
	d := dataOf[contract.AlarmStateData](t, st[len(st)-1])
	if d.NextScheduled == nil || !d.NextScheduled.Equal(t0.Add(time.Hour)) {
		t.Fatalf("%+v", d)
	}
	r.validateAll()
}
