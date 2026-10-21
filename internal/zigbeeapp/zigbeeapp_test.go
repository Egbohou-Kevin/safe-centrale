package zigbeeapp

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/inbox"
	"safehome/internal/outbox/outboxtest"
	"safehome/internal/zigbee"
)

func TestTheStartPublishesTheRegistryAndTheStatus(t *testing.T) {
	r := newRig(t, nil)
	r.settle()
	reg := r.broker.OfType(contract.TypeRegistrySnapshot)
	st := r.broker.OfType(contract.TypeZigbeeStatus)
	if len(reg) != 1 || !reg[0].Retain || reg[0].Topic != contract.RegistryDevicesTopic(site) || len(r.registry().Devices) != 0 {
		t.Fatalf("%+v", reg)
	}
	if !st[0].Retain || st[0].Topic != contract.ZigbeeStatusTopic(site) || r.status().Bridge != contract.BridgeOffline {
		t.Fatalf("%+v", r.status())
	}
	r.z2m("bridge/state", `{"state":"online"}`, true)
	drivertest.WaitFor(t, "the bridge online", func() bool { return r.status().Bridge == contract.BridgeOnline })
	r.validateAll()
}

func TestPairingADevice(t *testing.T) {
	r := newRig(t, nil)
	r.pair("dev_door", "0x01", "MCCGQ11LM")
	r.settle()
	if pj := r.rawOn("bridge/request/permit_join"); len(pj) != 1 || pj[0]["time"] != 120.0 {
		t.Fatalf("%+v", pj)
	}
	if rn := r.rawOn("bridge/request/device/rename"); len(rn) != 1 || rn[0]["from"] != "0x01" || rn[0]["to"] != "dev_door" {
		t.Fatalf("%+v", rn)
	}
	added := r.broker.OfType(contract.TypeRegistryDeviceAdded)
	d, _ := contract.DecodeData[contract.RegistryDeviceAddedData](added[0].Env)
	if d.DeviceID != "dev_door" || d.Model != "aqara.contact.mccgq11lm" || !slices.Equal(d.Capabilities, []string{"battery", "contact"}) {
		t.Fatalf("%+v", d)
	}
	if !r.status().Pairing || r.status().Devices != 1 {
		t.Fatalf("%+v", r.status())
	}
	// The address never leaves the adapter.
	for _, m := range r.broker.All() {
		if m.IsEnv && strings.Contains(string(m.Payload), "0x01") {
			t.Fatalf("%s carries the address", m.Env.Type)
		}
	}
	// An unknown model is refused and expelled.
	r.z2m("bridge/event", `{"type":"device_interview","data":{"ieee_address":"0x09","status":"successful","supported":true,"definition":{"model":"WXKG01LM"}}}`, false)
	drivertest.WaitFor(t, "the refusal", func() bool { return len(r.broker.OfType(contract.TypeRegistryDeviceRejected)) == 1 })
	r.settle()
	if rm := r.rawOn("bridge/request/device/remove"); len(rm) != 1 || rm[0]["id"] != "0x09" {
		t.Fatalf("%+v", rm)
	}
	// The window closes after its time.
	r.clock.Advance(2 * time.Minute)
	drivertest.WaitFor(t, "the window closed", func() bool { return !r.status().Pairing })
	r.validateAll()
}

func TestDeviceMessagesBecomeEventsAndStates(t *testing.T) {
	r := newRig(t, nil)
	r.pair("dev_door", "0x01", "MCCGQ11LM")
	r.z2m("dev_door", `{"contact":true,"battery":90,"linkquality":100}`, false)
	r.settle()
	if len(r.broker.On(contract.DeviceEventTopic(site, "dev_door"))) != 1 {
		t.Fatal("only the battery is an event at first: the first door state is not a change")
	}
	contact := r.broker.On(contract.DeviceStateTopic(site, "dev_door", "contact"))
	if len(contact) != 1 || !contact[0].Retain {
		t.Fatalf("%+v", contact)
	}
	r.z2m("dev_door", `{"contact":false,"battery":90,"linkquality":100}`, false)
	drivertest.WaitFor(t, "the door event", func() bool { return len(r.broker.On(contract.DeviceEventTopic(site, "dev_door"))) == 2 })
	ev := r.broker.On(contract.DeviceEventTopic(site, "dev_door"))[1]
	d, _ := contract.DecodeData[contract.DeviceEventData](ev.Env)
	if d.Capability != "contact" || d.StringValue() != "open" || ev.Retain {
		t.Fatalf("%+v", d)
	}
	r.validateAll()
}

func TestASirenCommandIsConfirmed(t *testing.T) {
	r := newRig(t, nil)
	r.pair("dev_siren", "0x02", "HS2WD-E")
	exp := r.clock.Now().Add(30 * time.Second)
	ref := r.order(contract.DeviceCmdTopic(site, "dev_siren"), contract.TypeCommand,
		contract.DeviceCommandData{DeviceID: "dev_siren", Command: contract.CmdSirenOn, Args: contract.DeviceCommandArgs{DurationS: 300}, Exp: contract.TSPtr(exp)})
	drivertest.WaitFor(t, "the request to the siren", func() bool { return len(r.rawOn("0x02/set")) == 1 })
	if w := r.rawOn("0x02/set")[0]["warning"].(map[string]any); w["duration"] != 300.0 || w["mode"] != "burglar" {
		t.Fatalf("%+v", w)
	}
	r.z2m("dev_siren", `{"alarm":"ON"}`, false)
	drivertest.WaitFor(t, "done", func() bool { return len(r.broker.On(contract.DeviceAckTopic(site, "dev_siren"))) == 2 })
	var statuses []string
	for _, m := range r.broker.On(contract.DeviceAckTopic(site, "dev_siren")) {
		a, _ := contract.DecodeData[contract.CommandAckData](m.Env)
		if a.Ref != ref {
			t.Fatalf("%+v", a)
		}
		statuses = append(statuses, a.Status)
	}
	if !slices.Equal(statuses, []string{contract.CmdAccepted, contract.CmdDone}) {
		t.Fatal(statuses)
	}
	// Not confirmed: sent again, then failed.
	r.order(contract.DeviceCmdTopic(site, "dev_siren"), contract.TypeCommand, contract.DeviceCommandData{DeviceID: "dev_siren", Command: contract.CmdSirenOff})
	drivertest.WaitFor(t, "the request", func() bool { return len(r.rawOn("0x02/set")) == 2 })
	for i := range 3 {
		r.clock.WaitTimer(r.t, r.clock.Now().Add(3*time.Second))
		r.clock.Advance(3 * time.Second)
		if i < 2 {
			drivertest.WaitFor(r.t, "sent again", func() bool { return len(r.rawOn("0x02/set")) == 3+i })
		}
	}
	drivertest.WaitFor(t, "failed", func() bool { return len(r.broker.On(contract.DeviceAckTopic(site, "dev_siren"))) == 4 })
	r.validateAll()
}

func TestRemovingADeviceClearsItsStates(t *testing.T) {
	r := newRig(t, nil)
	r.pair("dev_door", "0x01", "MCCGQ11LM")
	r.z2m("dev_door", `{"contact":false,"battery":90}`, false)
	r.settle()
	ref := r.order(contract.RegistryCmdTopic(site), contract.TypeRegistryRemove, contract.RegistryRemoveData{DeviceID: "dev_door"})
	drivertest.WaitFor(t, "removed", func() bool { return len(r.broker.OfType(contract.TypeRegistryDeviceRemoved)) == 1 })
	r.settle()
	d, _ := contract.DecodeData[contract.RegistryDeviceRemovedData](r.broker.OfType(contract.TypeRegistryDeviceRemoved)[0].Env)
	if d.Ref != ref || d.DeviceID != "dev_door" || len(r.registry().Devices) != 0 {
		t.Fatalf("%+v", d)
	}
	if rm := r.rawOn("bridge/request/device/remove"); len(rm) != 1 || rm[0]["id"] != "0x01" {
		t.Fatalf("%+v", rm)
	}
	for _, c := range []string{"availability", "battery", "contact"} {
		msgs := r.broker.On(contract.DeviceStateTopic(site, "dev_door", c))
		last := msgs[len(msgs)-1]
		if !last.Retain || len(last.Payload) != 0 {
			t.Fatalf("%s: the retained state must be cleared: %+v", c, last)
		}
	}
	r.validateAll()
}

func TestAKeypadCodeIsPublishedButNeverLogged(t *testing.T) {
	r := newRig(t, nil)
	r.pair("dev_kp", "0x03", "KP-1")
	r.z2m("dev_kp", `{"action":"disarm","action_code":"482915"}`, false)
	drivertest.WaitFor(t, "the keypad event", func() bool { return len(r.broker.On(contract.DeviceEventTopic(site, "dev_kp"))) == 1 })
	d, _ := contract.DecodeData[contract.DeviceEventData](r.broker.On(contract.DeviceEventTopic(site, "dev_kp"))[0].Env)
	if d.Code != "482915" || d.StringValue() != "disarm" {
		t.Fatalf("%+v", d)
	}
	// A code that is not digits is refused, and still not logged.
	r.z2m("dev_kp", `{"action":"disarm","action_code":"48x915"}`, false)
	r.settle()
	if len(r.broker.On(contract.DeviceEventTopic(site, "dev_kp"))) != 1 {
		t.Fatal("refused")
	}
	if l := r.logs.String(); strings.Contains(l, "482915") || strings.Contains(l, "48x915") {
		t.Fatalf("a code in the logs:\n%s", l)
	}
	r.validateAll()
}

// Zigbee2MQTT publishes at QoS 1 to the adapter's lasting session: keys pressed while
// the adapter was down arrive when it comes back. Their event keeps the time the key
// was heard (last_seen), so the hub lets an old one expire.
func TestAKeyPressedWhileTheAdapterWasDownKeepsItsTime(t *testing.T) {
	r := newRig(t, nil)
	r.pair("dev_kp", "0x03", "KP-1")
	heard := r.clock.Now().Add(-2 * time.Hour)
	r.z2m("dev_kp", `{"action":"disarm","action_code":"482915","last_seen":"`+heard.Format(time.RFC3339)+`"}`, false)
	drivertest.WaitFor(t, "the keypad event", func() bool { return len(r.broker.On(contract.DeviceEventTopic(site, "dev_kp"))) == 1 })
	if ts := r.broker.On(contract.DeviceEventTopic(site, "dev_kp"))[0].Env.TS.Time; !ts.Equal(heard) {
		t.Fatalf("event written at %v, the key was heard at %v", ts, heard)
	}
	r.validateAll()
}

func TestInvalidMessagesChangeNothing(t *testing.T) {
	r := newRig(t, nil)
	r.pair("dev_siren", "0x02", "HS2WD-E")
	r.settle()
	before := len(r.broker.All())
	cmd := contract.DeviceCommandData{DeviceID: "dev_siren", Command: contract.CmdSirenOff}
	r.order(contract.DeviceCmdTopic(site, "dev_other"), contract.TypeCommand, cmd)                        // topic and content disagree
	r.order(contract.DeviceCmdTopic(site, "dev_siren"), contract.TypeAlarmAck, cmd)                       // wrong type
	r.order(contract.DeviceCmdTopic(site, "dev_siren"), contract.TypeCommand, map[string]any{"x": 1})     // invalid command
	r.order(contract.RegistryCmdTopic(site), contract.TypeRegistryPair, map[string]any{"duration_s": -5}) // invalid pairing
	r.order(contract.RegistryCmdTopic(site), contract.TypeRegistryRemove, map[string]any{})               // invalid removal
	r.order(contract.RegistryCmdTopic(site), contract.TypeCommand, cmd)                                   // wrong type
	// Not an envelope:
	r.app.HandleMessage(inbox.Message{Topic: contract.DeviceCmdTopic(site, "dev_siren"), Payload: []byte("{")})
	r.app.HandleMessage(inbox.Message{Topic: contract.DeviceCmdTopic("tg-lom-0002", "dev_siren"), Payload: []byte("{}")})
	r.app.HandleMessage(inbox.Message{Topic: "safehome/" + site + "/alarm/cmd", Payload: []byte("{}")})
	r.app.HandleMessage(inbox.Message{Topic: "zigbee2mqtt/dev_siren", Payload: bytes.Repeat([]byte("x"), maxPayload+1)})
	r.z2m("dev_siren", `[1]`, false)
	r.z2m("bridge/response/device/rename", `{"status":"error","error":"no such device"}`, false)
	r.settle()
	if got := len(r.broker.All()); got != before {
		t.Fatalf("%d messages published", got-before)
	}
	if !strings.Contains(r.logs.String(), "no such device") {
		t.Fatal("an error from Zigbee2MQTT is logged")
	}
	// The same order twice is carried out once.
	env, _ := contract.NewEnvelope(site, "test", 1, r.ids.New(), r.clock.Now(), contract.TypeCommand, cmd)
	b, _ := json.Marshal(env)
	r.app.HandleMessage(inbox.Message{Topic: contract.DeviceCmdTopic(site, "dev_siren"), Payload: b})
	r.app.HandleMessage(inbox.Message{Topic: contract.DeviceCmdTopic(site, "dev_siren"), Payload: b})
	r.settle()
	if len(r.rawOn("0x02/set")) != 1 {
		t.Fatal("a duplicate is dropped")
	}
}

func TestTheStatusIsRepeatedAndTheRegistrySurvivesARestart(t *testing.T) {
	const every = 47 * time.Second
	store := drivertest.NewMemStore()
	r := newRig(t, store, func(c *Config) { c.StatusEvery = every })
	r.pair("dev_door", "0x01", "MCCGQ11LM")
	r.settle()
	before := len(r.broker.OfType(contract.TypeZigbeeStatus))
	r.clock.WaitTimer(t, t0.Add(every))
	r.clock.Advance(every)
	drivertest.WaitFor(t, "the repeated status", func() bool { return len(r.broker.OfType(contract.TypeZigbeeStatus)) == before+1 })
	r.stop()

	again := newRig(t, store)
	again.settle()
	if reg := again.registry(); len(reg.Devices) != 1 || reg.Devices[0].DeviceID != "dev_door" {
		t.Fatalf("%+v", reg)
	}
	again.z2m("dev_door", `{"contact":false}`, true) // retained at start: a state, no event
	again.settle()
	if len(again.broker.On(contract.DeviceEventTopic(site, "dev_door"))) != 0 {
		t.Fatal("no event from a retained message")
	}
}

func TestConfigurationAndDependenciesAreChecked(t *testing.T) {
	good := DefaultConfig(site, zigbee.DefaultConfig(profiles()))
	deps := Deps{Clock: drivertest.NewFakeClock(t0), Store: drivertest.NewMemStore(),
		Publisher: &outboxtest.Broker{}, NewID: contract.NewIDGenerator().New, NewDeviceID: func() string { return "dev_x" }}
	if _, err := New(good, deps); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]func(*Config){
		"site":          func(c *Config) { c.Site = "Lome" },
		"base":          func(c *Config) { c.Base = "" },
		"wildcard":      func(c *Config) { c.Base = "z2m/#" },
		"slash":         func(c *Config) { c.Base = "z2m/" },
		"contract root": func(c *Config) { c.Base = "safehome" },
		"contract site": func(c *Config) { c.Base = "safehome/" + site },
		"outbox":        func(c *Config) { c.Outbox.Size = 0 },
		"status every":  func(c *Config) { c.StatusEvery = 0 },
		"core":          func(c *Config) { c.Zigbee.Attempts = 0 },
	} {
		bad := good
		m(&bad)
		if _, err := New(bad, deps); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	noIDs := deps
	noIDs.NewDeviceID = nil
	if _, err := New(good, noIDs); err == nil {
		t.Error("device ids are required")
	}
	a, _ := New(good, deps)
	want := []string{"zigbee2mqtt/#", "safehome/" + site + "/device/+/cmd", "safehome/" + site + "/registry/cmd"}
	var got []string
	for _, s := range a.Subscriptions() {
		got = append(got, s.Filter)
		if s.QoS != 1 {
			t.Errorf("%+v", s)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err == nil {
		t.Fatal("Run twice")
	}
}

func TestAFailedActionDoesNotHoldBackTheOthers(t *testing.T) {
	r := newRig(t, nil)
	r.pair("dev_siren", "0x02", "HS2WD-E")
	r.settle()
	err := r.app.loop.Do(context.Background(), func(time.Time) []zigbee.Action {
		return []zigbee.Action{
			zigbee.Ack{Ref: "c1", DeviceID: "dev_siren", Status: "maybe"}, // cannot be built
			zigbee.Send{IEEE: "0x02", Payload: map[string]any{"warning": map[string]any{"mode": "stop"}}},
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	r.settle()
	if len(r.rawOn("0x02/set")) != 1 {
		t.Fatal("the order to the siren must still go out")
	}
	if !strings.Contains(r.logs.String(), "maybe") {
		t.Fatal("the failure is logged")
	}
}

// A message that arrives while the adapter stops (here during its last flush, which
// the broker holds) is given up without an error.
func TestMessagesDuringShutdownAreGivenUpQuietly(t *testing.T) {
	r := newRig(t, nil)
	r.settle()
	release := r.broker.Hold()
	r.app.repeatStatus(context.Background()) // something left for the last flush
	r.cancel()
	drivertest.WaitFor(t, "the loop to stop", func() bool { return r.app.Alive(context.Background()) != nil })
	r.order(contract.RegistryCmdTopic(site), contract.TypeRegistryPair, contract.RegistryPairData{DurationS: 120})
	r.z2m("bridge/state", `{"state":"online"}`, false)
	release()
	<-r.done
	if strings.Contains(r.logs.String(), "level=ERROR") {
		t.Fatal("an error was logged for a message given up at shutdown")
	}
}

// The broker may deliver an order twice (QoS 1): it is carried out once.
func TestARegistryOrderDeliveredTwiceIsCarriedOutOnce(t *testing.T) {
	r := newRig(t, nil)
	env, _ := contract.NewEnvelope(site, "test", 1, r.ids.New(), r.clock.Now(), contract.TypeRegistryPair, contract.RegistryPairData{DurationS: 120})
	b, _ := json.Marshal(env)
	for range 2 {
		r.app.HandleMessage(inbox.Message{Topic: contract.RegistryCmdTopic(site), Payload: b})
	}
	r.settle()
	if pj := r.rawOn("bridge/request/permit_join"); len(pj) != 1 {
		t.Fatalf("%d pairing requests", len(pj))
	}
}
