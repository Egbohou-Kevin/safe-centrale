package zigbeeapp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/schematest"
	"safehome/internal/zigbee"
)

const site = "tg-lom-0001"

var t0 = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

// --- fakes ---------------------------------------------------------------------

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*fakeTimer]bool
}

type fakeTimer struct {
	when time.Time
	c    chan time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{when: c.now.Add(d), c: make(chan time.Time, 1)}
	if d <= 0 {
		t.c <- c.now
		return t.c, func() {}
	}
	c.timers[t] = true
	return t.c, func() { c.mu.Lock(); delete(c.timers, t); c.mu.Unlock() }
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for t := range c.timers {
		if !t.when.After(c.now) {
			t.c <- c.now
			delete(c.timers, t)
		}
	}
}

func (c *fakeClock) hasTimerAt(when time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for t := range c.timers {
		if t.when.Equal(when) {
			return true
		}
	}
	return false
}

type memStore struct {
	mu   sync.Mutex
	docs map[string][]byte
}

func (s *memStore) Save(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[name] = append([]byte(nil), data...)
	return nil
}

func (s *memStore) Load(name string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.docs[name]
	return d, ok, nil
}

type published struct {
	topic  string
	retain bool
	raw    []byte
	env    contract.Envelope
	isEnv  bool
}

type fakeBroker struct {
	mu   sync.Mutex
	msgs []published
}

func (b *fakeBroker) Publish(_ context.Context, topic string, payload []byte, _ byte, retain bool) error {
	p := published{topic: topic, retain: retain, raw: append([]byte(nil), payload...)}
	if env, err := contract.ParseEnvelope(payload); err == nil {
		p.env, p.isEnv = env, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, p)
	return nil
}

func (b *fakeBroker) all() []published {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]published(nil), b.msgs...)
}

func (b *fakeBroker) ofType(typ string) []published {
	var out []published
	for _, m := range b.all() {
		if m.isEnv && m.env.Type == typ {
			out = append(out, m)
		}
	}
	return out
}

func (b *fakeBroker) on(topic string) []published {
	var out []published
	for _, m := range b.all() {
		if m.topic == topic {
			out = append(out, m)
		}
	}
	return out
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// --- profiles ------------------------------------------------------------------

func profiles() []zigbee.Profile {
	return []zigbee.Profile{
		{
			Name: "aqara.contact.mccgq11lm", Models: []string{"MCCGQ11LM"},
			Capabilities: map[string]zigbee.Rule{
				"contact": {From: "contact", Map: map[string]string{"true": "closed", "false": "open"}},
				"battery": {From: "battery"},
			},
		},
		{
			Name: "heiman.siren.hs2wd", Models: []string{"HS2WD-E"},
			Capabilities: map[string]zigbee.Rule{"siren": {From: "alarm", Map: map[string]string{"ON": "on", "OFF": "off"}}},
			Commands: map[string]zigbee.ProfileCommand{
				"siren.on":  {Payload: map[string]any{"warning": map[string]any{"mode": "burglar", "duration": "{duration_s}"}}, Confirm: &zigbee.Expect{Capability: "siren", Value: "on"}},
				"siren.off": {Payload: map[string]any{"warning": map[string]any{"mode": "stop"}}, Confirm: &zigbee.Expect{Capability: "siren", Value: "off"}},
			},
		},
		{
			Name: "tuya.keypad.example", Models: []string{"KP-1"},
			Capabilities: map[string]zigbee.Rule{
				"keypad": {From: "action", Map: map[string]string{"disarm": "disarm", "arm_all_zones": "arm"}, Momentary: true, CodeFrom: "action_code"},
			},
		},
	}
}

// --- rig -----------------------------------------------------------------------

type rig struct {
	t      *testing.T
	app    *App
	clock  *fakeClock
	store  *memStore
	broker *fakeBroker
	logs   *syncBuffer
	ids    *contract.IDGenerator
	devIDs chan string
	cancel context.CancelFunc
	done   chan struct{}
}

func newRig(t *testing.T, store *memStore, mutate ...func(*Config)) *rig {
	t.Helper()
	if store == nil {
		store = &memStore{docs: map[string][]byte{}}
	}
	r := &rig{
		t: t, clock: &fakeClock{now: t0, timers: map[*fakeTimer]bool{}}, store: store, broker: &fakeBroker{},
		logs: &syncBuffer{}, ids: contract.NewIDGenerator(), devIDs: make(chan string, 10),
	}
	cfg := DefaultConfig(site, zigbee.DefaultConfig(profiles()))
	cfg.RetryMin, cfg.RetryMax = time.Millisecond, 5*time.Millisecond
	for _, m := range mutate {
		m(&cfg)
	}
	app, err := New(cfg, Deps{
		Clock: r.clock, Store: store, Publisher: r.broker, NewID: r.ids.New,
		NewDeviceID: func() string {
			select {
			case id := <-r.devIDs:
				return id
			default:
				return ""
			}
		},
		Logger: slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.app = app
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	go func() { app.Run(ctx); close(r.done) }()
	t.Cleanup(r.stop)
	r.waitFor("the first status", func() bool { return len(r.broker.ofType(contract.TypeZigbeeStatus)) > 0 })
	return r
}

var stopOnce sync.Map

func (r *rig) stop() {
	if _, done := stopOnce.LoadOrStore(r, true); done {
		return
	}
	r.cancel()
	<-r.done
}

func (r *rig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s\nlogs:\n%s", what, r.logs.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// settle waits until the loop has handled everything handed to it so far.
func (r *rig) settle() {
	r.t.Helper()
	r.app.loop.Do(context.Background(), func(time.Time) []zigbee.Action { return nil })
	r.waitFor("the outbox to empty", func() bool { return r.app.out.Depth() == 0 })
}

// z2m delivers a Zigbee2MQTT message.
func (r *rig) z2m(topic, payload string, retain bool) {
	r.app.HandleMessage(Message{Topic: "zigbee2mqtt/" + topic, Payload: []byte(payload), Retain: retain})
}

// order sends a contract message and returns its id.
func (r *rig) order(topic, typ string, data any) string {
	r.t.Helper()
	id := r.ids.New()
	env, err := contract.NewEnvelope(site, "test", 1, id, r.clock.Now(), typ, data)
	if err != nil {
		r.t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	r.app.HandleMessage(Message{Topic: topic, Payload: b})
	return id
}

func (r *rig) status() contract.ZigbeeStatusData {
	all := r.broker.ofType(contract.TypeZigbeeStatus)
	d, _ := contract.DecodeData[contract.ZigbeeStatusData](all[len(all)-1].env)
	return d
}

func (r *rig) registry() contract.RegistrySnapshotData {
	all := r.broker.ofType(contract.TypeRegistrySnapshot)
	d, _ := contract.DecodeData[contract.RegistrySnapshotData](all[len(all)-1].env)
	return d
}

// rawOn returns the bodies sent on a Zigbee2MQTT topic.
func (r *rig) rawOn(topic string) []map[string]any {
	var out []map[string]any
	for _, m := range r.broker.on("zigbee2mqtt/" + topic) {
		var v map[string]any
		json.Unmarshal(m.raw, &v)
		out = append(out, v)
	}
	return out
}

// pair adds a device through the whole pairing sequence.
func (r *rig) pair(id, ieee, model string) {
	r.t.Helper()
	r.devIDs <- id
	r.order(contract.RegistryCmdTopic(site), contract.TypeRegistryPair, contract.RegistryPairData{DurationS: 120})
	r.z2m("bridge/event", `{"type":"device_interview","data":{"ieee_address":"`+ieee+`","status":"successful","supported":true,"definition":{"model":"`+model+`"}}}`, false)
	r.waitFor(id+" added", func() bool {
		return slices.ContainsFunc(r.registry().Devices, func(d contract.RegistryDevice) bool { return d.DeviceID == id })
	})
}

var schemaOf = map[string]string{
	contract.TypeDeviceEvent: "device-event.json", contract.TypeDeviceState: "device-event.json",
	contract.TypeCommandAck: "device-command-ack.json", contract.TypeRegistrySnapshot: "registry-snapshot.json",
	contract.TypeRegistryDeviceAdded: "registry-device-added.json", contract.TypeRegistryDeviceRejected: "registry-device-rejected.json",
	contract.TypeRegistryDeviceRemoved: "registry-device-removed.json", contract.TypeZigbeeStatus: "zigbee-status.json",
}

// validateAll checks every contract message against its schema.
func (r *rig) validateAll() {
	r.t.Helper()
	for _, m := range r.broker.all() {
		if !m.isEnv {
			continue
		}
		file, ok := schemaOf[m.env.Type]
		if !ok {
			r.t.Errorf("unexpected type %s", m.env.Type)
			continue
		}
		if err := schematest.Check(file, m.env.Data); err != nil {
			r.t.Errorf("%s: %v", m.env.Type, err)
		}
		if err := schematest.Check("envelope.json", m.raw); err != nil {
			r.t.Errorf("envelope: %v", err)
		}
		if m.env.Src != contract.AdapterZigbee {
			r.t.Errorf("src %q", m.env.Src)
		}
	}
}

// --- tests ---------------------------------------------------------------------

func TestTheStartPublishesTheRegistryAndTheStatus(t *testing.T) {
	r := newRig(t, nil)
	r.settle()
	reg := r.broker.ofType(contract.TypeRegistrySnapshot)
	st := r.broker.ofType(contract.TypeZigbeeStatus)
	if len(reg) != 1 || !reg[0].retain || reg[0].topic != contract.RegistryDevicesTopic(site) || len(r.registry().Devices) != 0 {
		t.Fatalf("%+v", reg)
	}
	if !st[0].retain || st[0].topic != contract.ZigbeeStatusTopic(site) || r.status().Bridge != contract.BridgeOffline {
		t.Fatalf("%+v", r.status())
	}
	r.z2m("bridge/state", `{"state":"online"}`, true)
	r.waitFor("the bridge online", func() bool { return r.status().Bridge == contract.BridgeOnline })
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
	added := r.broker.ofType(contract.TypeRegistryDeviceAdded)
	d, _ := contract.DecodeData[contract.RegistryDeviceAddedData](added[0].env)
	if d.DeviceID != "dev_door" || d.Model != "aqara.contact.mccgq11lm" || !slices.Equal(d.Capabilities, []string{"battery", "contact"}) {
		t.Fatalf("%+v", d)
	}
	if !r.status().Pairing || r.status().Devices != 1 {
		t.Fatalf("%+v", r.status())
	}
	// The address never leaves the adapter.
	for _, m := range r.broker.all() {
		if m.isEnv && strings.Contains(string(m.raw), "0x01") {
			t.Fatalf("%s carries the address", m.env.Type)
		}
	}
	// An unknown model is refused and expelled.
	r.z2m("bridge/event", `{"type":"device_interview","data":{"ieee_address":"0x09","status":"successful","supported":true,"definition":{"model":"WXKG01LM"}}}`, false)
	r.waitFor("the refusal", func() bool { return len(r.broker.ofType(contract.TypeRegistryDeviceRejected)) == 1 })
	r.settle()
	if rm := r.rawOn("bridge/request/device/remove"); len(rm) != 1 || rm[0]["id"] != "0x09" {
		t.Fatalf("%+v", rm)
	}
	// The window closes after its time.
	r.clock.Advance(2 * time.Minute)
	r.waitFor("the window closed", func() bool { return !r.status().Pairing })
	r.validateAll()
}

func TestDeviceMessagesBecomeEventsAndStates(t *testing.T) {
	r := newRig(t, nil)
	r.pair("dev_door", "0x01", "MCCGQ11LM")
	r.z2m("dev_door", `{"contact":true,"battery":90,"linkquality":100}`, false)
	r.settle()
	if len(r.broker.on(contract.DeviceEventTopic(site, "dev_door"))) != 1 {
		t.Fatal("only the battery is an event at first: the first door state is not a change")
	}
	contact := r.broker.on(contract.DeviceStateTopic(site, "dev_door", "contact"))
	if len(contact) != 1 || !contact[0].retain {
		t.Fatalf("%+v", contact)
	}
	r.z2m("dev_door", `{"contact":false,"battery":90,"linkquality":100}`, false)
	r.waitFor("the door event", func() bool { return len(r.broker.on(contract.DeviceEventTopic(site, "dev_door"))) == 2 })
	ev := r.broker.on(contract.DeviceEventTopic(site, "dev_door"))[1]
	d, _ := contract.DecodeData[contract.DeviceEventData](ev.env)
	if d.Capability != "contact" || d.StringValue() != "open" || ev.retain {
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
	r.waitFor("the request to the siren", func() bool { return len(r.rawOn("0x02/set")) == 1 })
	if w := r.rawOn("0x02/set")[0]["warning"].(map[string]any); w["duration"] != 300.0 || w["mode"] != "burglar" {
		t.Fatalf("%+v", w)
	}
	r.z2m("dev_siren", `{"alarm":"ON"}`, false)
	r.waitFor("done", func() bool { return len(r.broker.on(contract.DeviceAckTopic(site, "dev_siren"))) == 2 })
	var statuses []string
	for _, m := range r.broker.on(contract.DeviceAckTopic(site, "dev_siren")) {
		a, _ := contract.DecodeData[contract.CommandAckData](m.env)
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
	r.waitFor("the request", func() bool { return len(r.rawOn("0x02/set")) == 2 })
	for i := range 3 {
		r.waitFor("the wait", func() bool { return r.clock.hasTimerAt(r.clock.Now().Add(3 * time.Second)) })
		r.clock.Advance(3 * time.Second)
		if i < 2 {
			r.waitFor("sent again", func() bool { return len(r.rawOn("0x02/set")) == 3+i })
		}
	}
	r.waitFor("failed", func() bool { return len(r.broker.on(contract.DeviceAckTopic(site, "dev_siren"))) == 4 })
	r.validateAll()
}

func TestRemovingADeviceClearsItsStates(t *testing.T) {
	r := newRig(t, nil)
	r.pair("dev_door", "0x01", "MCCGQ11LM")
	r.z2m("dev_door", `{"contact":false,"battery":90}`, false)
	r.settle()
	ref := r.order(contract.RegistryCmdTopic(site), contract.TypeRegistryRemove, contract.RegistryRemoveData{DeviceID: "dev_door"})
	r.waitFor("removed", func() bool { return len(r.broker.ofType(contract.TypeRegistryDeviceRemoved)) == 1 })
	r.settle()
	d, _ := contract.DecodeData[contract.RegistryDeviceRemovedData](r.broker.ofType(contract.TypeRegistryDeviceRemoved)[0].env)
	if d.Ref != ref || d.DeviceID != "dev_door" || len(r.registry().Devices) != 0 {
		t.Fatalf("%+v", d)
	}
	if rm := r.rawOn("bridge/request/device/remove"); len(rm) != 1 || rm[0]["id"] != "0x01" {
		t.Fatalf("%+v", rm)
	}
	for _, c := range []string{"availability", "battery", "contact"} {
		msgs := r.broker.on(contract.DeviceStateTopic(site, "dev_door", c))
		last := msgs[len(msgs)-1]
		if !last.retain || len(last.raw) != 0 {
			t.Fatalf("%s: the retained state must be cleared: %+v", c, last)
		}
	}
	r.validateAll()
}

func TestAKeypadCodeIsPublishedButNeverLogged(t *testing.T) {
	r := newRig(t, nil)
	r.pair("dev_kp", "0x03", "KP-1")
	r.z2m("dev_kp", `{"action":"disarm","action_code":"482915"}`, false)
	r.waitFor("the keypad event", func() bool { return len(r.broker.on(contract.DeviceEventTopic(site, "dev_kp"))) == 1 })
	d, _ := contract.DecodeData[contract.DeviceEventData](r.broker.on(contract.DeviceEventTopic(site, "dev_kp"))[0].env)
	if d.Code != "482915" || d.StringValue() != "disarm" {
		t.Fatalf("%+v", d)
	}
	// A code that is not digits is refused, and still not logged.
	r.z2m("dev_kp", `{"action":"disarm","action_code":"48x915"}`, false)
	r.settle()
	if len(r.broker.on(contract.DeviceEventTopic(site, "dev_kp"))) != 1 {
		t.Fatal("refused")
	}
	if l := r.logs.String(); strings.Contains(l, "482915") || strings.Contains(l, "48x915") {
		t.Fatalf("a code in the logs:\n%s", l)
	}
	r.validateAll()
}

func TestInvalidMessagesChangeNothing(t *testing.T) {
	r := newRig(t, nil)
	r.pair("dev_siren", "0x02", "HS2WD-E")
	r.settle()
	before := len(r.broker.all())
	cmd := contract.DeviceCommandData{DeviceID: "dev_siren", Command: contract.CmdSirenOff}
	r.order(contract.DeviceCmdTopic(site, "dev_other"), contract.TypeCommand, cmd)                        // topic and content disagree
	r.order(contract.DeviceCmdTopic(site, "dev_siren"), contract.TypeAlarmAck, cmd)                       // wrong type
	r.order(contract.DeviceCmdTopic(site, "dev_siren"), contract.TypeCommand, map[string]any{"x": 1})     // invalid command
	r.order(contract.RegistryCmdTopic(site), contract.TypeRegistryPair, map[string]any{"duration_s": -5}) // invalid pairing
	r.order(contract.RegistryCmdTopic(site), contract.TypeRegistryRemove, map[string]any{})               // invalid removal
	r.order(contract.RegistryCmdTopic(site), contract.TypeCommand, cmd)                                   // wrong type
	r.app.HandleMessage(Message{Topic: contract.DeviceCmdTopic(site, "dev_siren"), Payload: []byte("{")}) // not an envelope
	r.app.HandleMessage(Message{Topic: contract.DeviceCmdTopic("tg-lom-0002", "dev_siren"), Payload: []byte("{}")})
	r.app.HandleMessage(Message{Topic: "safehome/" + site + "/alarm/cmd", Payload: []byte("{}")})
	r.app.HandleMessage(Message{Topic: "zigbee2mqtt/dev_siren", Payload: bytes.Repeat([]byte("x"), maxPayload+1)})
	r.z2m("dev_siren", `[1]`, false)
	r.z2m("bridge/response/device/rename", `{"status":"error","error":"no such device"}`, false)
	r.settle()
	if got := len(r.broker.all()); got != before {
		t.Fatalf("%d messages published", got-before)
	}
	if !strings.Contains(r.logs.String(), "no such device") {
		t.Fatal("an error from Zigbee2MQTT is logged")
	}
	// The same order twice is carried out once.
	env, _ := contract.NewEnvelope(site, "test", 1, r.ids.New(), r.clock.Now(), contract.TypeCommand, cmd)
	b, _ := json.Marshal(env)
	r.app.HandleMessage(Message{Topic: contract.DeviceCmdTopic(site, "dev_siren"), Payload: b})
	r.app.HandleMessage(Message{Topic: contract.DeviceCmdTopic(site, "dev_siren"), Payload: b})
	r.settle()
	if len(r.rawOn("0x02/set")) != 1 {
		t.Fatal("a duplicate is dropped")
	}
}

func TestTheStatusIsRepeatedAndTheRegistrySurvivesARestart(t *testing.T) {
	const every = 47 * time.Second
	store := &memStore{docs: map[string][]byte{}}
	r := newRig(t, store, func(c *Config) { c.StatusEvery = every })
	r.pair("dev_door", "0x01", "MCCGQ11LM")
	r.settle()
	before := len(r.broker.ofType(contract.TypeZigbeeStatus))
	r.waitFor("the repeater", func() bool { return r.clock.hasTimerAt(t0.Add(every)) })
	r.clock.Advance(every)
	r.waitFor("the repeated status", func() bool { return len(r.broker.ofType(contract.TypeZigbeeStatus)) == before+1 })
	r.stop()

	again := newRig(t, store)
	again.settle()
	if reg := again.registry(); len(reg.Devices) != 1 || reg.Devices[0].DeviceID != "dev_door" {
		t.Fatalf("%+v", reg)
	}
	again.z2m("dev_door", `{"contact":false}`, true) // retained at start: a state, no event
	again.settle()
	if len(again.broker.on(contract.DeviceEventTopic(site, "dev_door"))) != 0 {
		t.Fatal("no event from a retained message")
	}
}

func TestConfigurationAndDependenciesAreChecked(t *testing.T) {
	good := DefaultConfig(site, zigbee.DefaultConfig(profiles()))
	deps := Deps{Clock: &fakeClock{now: t0, timers: map[*fakeTimer]bool{}}, Store: &memStore{docs: map[string][]byte{}},
		Publisher: &fakeBroker{}, NewID: contract.NewIDGenerator().New, NewDeviceID: func() string { return "dev_x" }}
	if _, err := New(good, deps); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]func(*Config){
		"site":         func(c *Config) { c.Site = "Lome" },
		"base":         func(c *Config) { c.Base = "" },
		"wildcard":     func(c *Config) { c.Base = "z2m/#" },
		"slash":        func(c *Config) { c.Base = "z2m/" },
		"outbox":       func(c *Config) { c.OutboxSize = 0 },
		"status every": func(c *Config) { c.StatusEvery = 0 },
		"core":         func(c *Config) { c.Zigbee.Attempts = 0 },
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
