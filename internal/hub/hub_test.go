package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"safehome/internal/access"
	"safehome/internal/contract"
	"safehome/internal/engine"
	"safehome/internal/escalation"
	"safehome/internal/health"
	"safehome/internal/schematest"
)

const (
	site = "tg-lom-0001"
	// Devices of the test shop.
	devDoor   = "dev_door"
	devBack   = "dev_back"
	devKeypad = "dev_keypad"
	devHub    = "dev_hubpower"
	devSiren  = "dev_siren"
	devMotion = "dev_motion"
)

var t0 = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

// --- fakes ----------------------------------------------------------------------

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*fakeTimer]bool
}

type fakeTimer struct {
	when time.Time
	c    chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: t0, timers: map[*fakeTimer]bool{}} }

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

type memStore struct {
	mu   sync.Mutex
	docs map[string][]byte
}

func newMemStore() *memStore { return &memStore{docs: map[string][]byte{}} }

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
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
	Env     contract.Envelope
}

type fakeBroker struct {
	mu       sync.Mutex
	msgs     []published
	failures int // the next n publishes fail as "unavailable"
	refuse   func(topic string) bool
	attempts int
}

func (b *fakeBroker) Publish(_ context.Context, topic string, payload []byte, qos byte, retain bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempts++
	if b.failures > 0 {
		b.failures--
		return ErrUnavailable
	}
	if b.refuse != nil && b.refuse(topic) {
		return errors.New("refused")
	}
	env, _ := contract.ParseEnvelope(payload)
	b.msgs = append(b.msgs, published{topic, append([]byte(nil), payload...), qos, retain, env})
	return nil
}

func (b *fakeBroker) all() []published {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]published(nil), b.msgs...)
}

func (b *fakeBroker) on(topic string) []published {
	var out []published
	for _, m := range b.all() {
		if m.Topic == topic {
			out = append(out, m)
		}
	}
	return out
}

func (b *fakeBroker) ofType(typ string) []published {
	var out []published
	for _, m := range b.all() {
		if m.Env.Type == typ {
			out = append(out, m)
		}
	}
	return out
}

// --- rig -------------------------------------------------------------------------

type rig struct {
	t      *testing.T
	hub    *Hub
	clock  *fakeClock
	store  *memStore
	broker *fakeBroker
	logs   *syncBuffer
	ids    *contract.IDGenerator
	cancel context.CancelFunc
	done   chan struct{}
	seq    int
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

type entropy struct{ n byte }

var rigCount atomic.Uint32

func (e *entropy) Read(p []byte) (int, error) {
	for i := range p {
		e.n++
		p[i] = e.n
	}
	return len(p), nil
}

func testConfig() Config {
	c := DefaultConfig(site, devHub, devSiren)
	c.Engine.Zones = map[string]engine.Zone{
		devDoor:   {Name: "entrance", Kind: engine.ZoneEntry},
		devBack:   {Name: "back", Kind: engine.ZoneInstant},
		devMotion: {Name: "shop", Kind: engine.ZoneInterior},
	}
	c.Access.Iterations = 10_000 // the minimum: fast tests
	c.Access.Rand = &entropy{}
	c.Escalation = escalation.DefaultConfig("Test Shop", []escalation.Contact{
		{ID: "u_owner", Name: "Owner", Role: escalation.RoleOwner, Channels: []escalation.Channel{escalation.ChannelApp, escalation.ChannelSMS}},
	})
	c.BootWindow = 0
	c.LinkWait = 0
	c.RetryMin, c.RetryMax = time.Millisecond, 5*time.Millisecond
	c.Versions = map[string]string{"engine": "0.1.0"}
	return c
}

func newRig(t *testing.T, store *memStore, mutate ...func(*Config)) *rig {
	t.Helper()
	r := &rig{t: t, clock: newFakeClock(), store: store, broker: &fakeBroker{}, logs: &syncBuffer{}}
	if store == nil {
		r.store = newMemStore()
	}
	cfg := testConfig()
	for _, m := range mutate {
		m(&cfg)
	}
	r.ids = contract.NewIDGeneratorWith(r.clock.Now, &entropy{n: byte(rigCount.Add(1) * 37)})
	h, err := New(cfg, Deps{
		Clock: r.clock, Store: r.store, Publisher: r.broker, NewID: r.ids.New,
		Logger: slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.hub = h
	return r
}

func (r *rig) start() {
	r.startWithoutWaiting()
	r.waitFor("boot", func() bool { return len(r.broker.on(contract.AlarmStateTopic(site))) >= 1 })
}

func (r *rig) startWithoutWaiting() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	go func() { r.hub.Run(ctx); close(r.done) }()
	r.t.Cleanup(r.stop)
}

func (r *rig) stop() {
	if r.cancel != nil {
		r.cancel()
		<-r.done
		r.cancel = nil
	}
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

// settle waits until the domains have processed what was queued and the outbox is empty.
func (r *rig) settle() {
	r.t.Helper()
	ctx := context.Background()
	if err := r.hub.engLoop.Do(ctx, func(time.Time) []engine.Action { return nil }); err != nil {
		r.t.Fatal(err)
	}
	if err := r.hub.monLoop.Do(ctx, func(time.Time) []health.Action { return nil }); err != nil {
		r.t.Fatal(err)
	}
	// The escalation is fed by the two loops above: it comes last.
	if err := r.hub.escLoop.Do(ctx, func(time.Time) []escalation.Action { return nil }); err != nil {
		r.t.Fatal(err)
	}
	r.waitFor("the outbox to empty", func() bool { return r.hub.out.Depth() == 0 })
}

// send gives the hub a message as the broker would.
func (r *rig) send(topic, typ string, data any) string {
	r.t.Helper()
	r.seq++
	id := r.ids.New()
	env, err := contract.NewEnvelope(site, "test", uint64(r.seq), id, r.clock.Now(), typ, data)
	if err != nil {
		r.t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	r.hub.HandleMessage(Message{Topic: topic, Payload: b})
	return id
}

func (r *rig) device(id, capability, value string) {
	r.t.Helper()
	r.send(contract.DeviceEventTopic(site, id), contract.TypeDeviceEvent, event(id, capability, value))
}

func event(id, capability, value string) contract.DeviceEventData {
	v := json.RawMessage(`"` + value + `"`)
	if capability == contract.CapBattery || capability == contract.CapLink {
		v = json.RawMessage(value)
	}
	return contract.DeviceEventData{DeviceID: id, Capability: capability, Value: v}
}

func (r *rig) keypad(value, code string) {
	r.t.Helper()
	d := event(devKeypad, contract.CapKeypad, value)
	d.Code = code
	r.send(contract.DeviceEventTopic(site, devKeypad), contract.TypeDeviceEvent, d)
}

func (r *rig) order(cmd string, args contract.CommandArgs, origin, user string) string {
	r.t.Helper()
	return r.send(contract.AlarmCmdTopic(site), contract.TypeAlarmCommand,
		contract.AlarmCommandData{Command: cmd, Args: args, Origin: origin, User: user})
}

func (r *rig) setCode(user, code string) {
	r.t.Helper()
	if err := r.hub.Codes().Set(user, code); err != nil {
		r.t.Fatal(err)
	}
}

// mode reads the engine's mode from the loop's goroutine.
func (r *rig) mode() (m engine.Mode) {
	r.t.Helper()
	if err := r.hub.engLoop.Do(context.Background(), func(time.Time) []engine.Action { m = r.hub.eng.Mode(); return nil }); err != nil {
		r.t.Fatal(err)
	}
	return m
}

// mustBe waits for the engine to reach the mode: ticks and code checks are handled
// by goroutines of their own, so the mode may take a moment to settle.
func (r *rig) mustBe(m engine.Mode) {
	r.t.Helper()
	r.waitFor("mode "+string(m), func() bool { r.settle(); return r.mode() == m })
}

// waitAcks waits until n acknowledgements have been published.
func (r *rig) waitAcks(n int) {
	r.t.Helper()
	r.waitFor(fmt.Sprintf("%d acks", n), func() bool { return len(r.broker.ofType(contract.TypeAlarmAck)) >= n })
}

// validateAll checks every published message against its JSON Schema.
func (r *rig) validateAll() {
	r.t.Helper()
	files := map[string]string{
		contract.TypeAlarmArming: "alarm-arming.json", contract.TypeAlarmArmed: "alarm-armed.json",
		contract.TypeAlarmDisarmed: "alarm-disarmed.json", contract.TypeAlarmEntryDelay: "alarm-entry-delay.json",
		contract.TypeAlarmTriggered: "alarm-triggered.json", contract.TypeAlarmSirenCycle: "alarm-siren-cycle.json",
		contract.TypeAlarmCleared: "alarm-cleared.json", contract.TypeAlarmAuthFailed: "alarm-auth-failed.json",
		contract.TypeAlarmArmingScheduled: "alarm-arming-scheduled.json", contract.TypeAlarmPostponed: "alarm-postponed.json",
		contract.TypeAlarmAck: "alarm-ack.json", contract.TypeAlarmState: "alarm-state.json",
		contract.TypeAlertRequest: "alert-request.json", contract.TypeCommand: "device-command.json",
		contract.TypeHeartbeat: "system-heartbeat.json", contract.TypeHealth: "system-health.json",
		contract.TypeInstallMode: "install-mode.json",
	}
	for _, m := range r.broker.all() {
		file, ok := files[m.Env.Type]
		if !ok {
			r.t.Errorf("no schema for published type %q", m.Env.Type)
			continue
		}
		if err := schematest.Check(file, m.Env.Data); err != nil {
			r.t.Errorf("%s on %s breaks %s: %v\n%s", m.Env.Type, m.Topic, file, err, m.Env.Data)
		}
		if err := schematest.Check("envelope.json", m.Payload); err != nil {
			r.t.Errorf("envelope of %s: %v", m.Env.Type, err)
		}
	}
}

func eventsOf(r *rig, typ string) []published { return r.broker.ofType(typ) }

func dataOf[T any](t *testing.T, p published) T {
	t.Helper()
	v, err := contract.DecodeData[T](p.Env)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// --- tests -------------------------------------------------------------------------

func TestBootPublishesTheRetainedStateAndHealth(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()

	st := r.broker.on(contract.AlarmStateTopic(site))
	if len(st) == 0 || !st[0].Retain || st[0].QoS != 1 || st[0].Env.Src != "engine" {
		t.Fatalf("alarm state: %+v", st)
	}
	if d := dataOf[contract.AlarmStateData](t, st[0]); d.State != contract.StateDisarmed {
		t.Fatalf("%+v", d)
	}
	h := r.broker.on(contract.HealthTopic(site))
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
	r.waitFor("armed", func() bool { return len(eventsOf(r, contract.TypeAlarmArmed)) == 1 })
	r.mustBe(engine.ModeArmed)

	r.device(devBack, contract.CapContact, "open") // perimeter door forced
	r.waitFor("the siren order", func() bool { return len(r.broker.on(contract.DeviceCmdTopic(site, devSiren))) == 1 })
	r.settle()

	siren := r.broker.on(contract.DeviceCmdTopic(site, devSiren))[0]
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
	if st := r.broker.on(contract.AlarmStateTopic(site)); dataOf[contract.AlarmStateData](t, st[len(st)-1]).State != contract.StateTriggered {
		t.Fatal("the retained state must follow the alarm")
	}

	// The owner disarms from the app, code checked on the hub.
	r.order(contract.CmdDisarm, contract.CommandArgs{Code: "482915"}, contract.OriginApp, "u_owner")
	r.waitFor("disarmed", func() bool { return len(eventsOf(r, contract.TypeAlarmDisarmed)) == 1 })
	r.mustBe(engine.ModeDisarmed)
	off := r.broker.on(contract.DeviceCmdTopic(site, devSiren))
	if len(off) != 2 || dataOf[contract.DeviceCommandData](t, off[1]).Command != contract.CmdSirenOff {
		t.Fatalf("siren orders: %d", len(off))
	}
	if d := dataOf[contract.DisarmedData](t, eventsOf(r, contract.TypeAlarmDisarmed)[0]); d.User != "u_owner" || d.Origin != contract.OriginApp {
		t.Fatalf("%+v", d)
	}
	r.settle()
	r.validateAll()
}

func TestTheSirenLeavesBeforeTheAlertAndTheEvents(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.order(contract.CmdArm, contract.CommandArgs{}, contract.OriginApp, "u")
	r.mustBe(engine.ModeArming)
	r.clock.Advance(2 * time.Minute)
	r.mustBe(engine.ModeArmed)
	before := len(r.broker.all())
	r.device(devBack, contract.CapContact, "open")
	r.settle()
	got := r.broker.all()[before:]
	var order []string
	for _, m := range got {
		order = append(order, m.Env.Type)
	}
	if len(order) < 3 || order[0] != contract.TypeCommand || !contains(order, contract.TypeAlertRequest) {
		t.Fatalf("order of departure: %v: the siren must leave first", order)
	}
}

func TestKeypadDisarmWithTheRightCode(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.setCode("u_guard", "730264")
	r.keypad(contract.ValArm, "")
	r.mustBe(engine.ModeArming)
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)

	r.device(devDoor, contract.CapContact, "open") // entry delay starts
	r.mustBe(engine.ModeEntryDelay)
	r.keypad(contract.ValDisarm, "730264")
	r.waitFor("disarmed", func() bool { return len(eventsOf(r, contract.TypeAlarmDisarmed)) == 1 })
	r.mustBe(engine.ModeDisarmed)
	d := dataOf[contract.DisarmedData](t, eventsOf(r, contract.TypeAlarmDisarmed)[0])
	if d.User != "u_guard" || d.Origin != contract.OriginKeypad {
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
	r.waitFor("three failures", func() bool { return len(eventsOf(r, contract.TypeAlarmAuthFailed)) == 3 })
	last := dataOf[contract.AuthFailedData](t, eventsOf(r, contract.TypeAlarmAuthFailed)[2])
	if last.Attempts != 3 || last.LockedUntil == nil {
		t.Fatalf("%+v", last)
	}
	// The escalation hears of the third failure after it is published: wait for it.
	r.waitFor("the auth_failed alert", func() bool {
		var kinds []string
		for _, a := range eventsOf(r, contract.TypeAlertRequest) {
			kinds = append(kinds, dataOf[contract.AlertRequestData](t, a).Kind)
		}
		return contains(kinds, contract.AlertAuthFailed)
	})

	// Locked: even the right code is refused now.
	acks := len(r.broker.ofType(contract.TypeAlarmAck))
	r.keypad(contract.ValDisarm, "482915")
	r.waitAcks(acks + 1)
	r.mustBe(engine.ModeArmed)
	if got := lastAck(t, r); got.Reason != contract.ReasonLockedOut {
		t.Fatalf("%+v", got)
	}

	// Nothing that was published or logged contains any of the codes.
	all := string(joinPayloads(r.broker.all())) + r.logs.String()
	for _, code := range []string{"482915", "111122", "222233", "333344"} {
		if strings.Contains(all, code) {
			t.Errorf("the code %s leaks into the messages or the logs", code)
		}
	}
	r.validateAll()
}

func lastAck(t *testing.T, r *rig) contract.AlarmAckData {
	t.Helper()
	acks := r.broker.ofType(contract.TypeAlarmAck)
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
	acks := r.broker.on(contract.AlarmAckTopic(site))
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
		r.hub.HandleMessage(Message{Topic: contract.AlarmCmdTopic(site), Payload: b})
	}
	r.settle()
	if n := len(r.broker.on(contract.AlarmAckTopic(site))); n != 1 {
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
	for name, m := range map[string]Message{
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
		before := len(r.broker.all())
		r.hub.HandleMessage(m)
		r.settle()
		if len(r.broker.all()) != before {
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
	r.waitFor("entry delay event", func() bool { return len(eventsOf(r, contract.TypeAlarmEntryDelay)) == 1 })
	r.clock.Advance(45 * time.Second)
	r.waitFor("the siren", func() bool { return len(r.broker.on(contract.DeviceCmdTopic(site, devSiren))) == 1 })
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
	r.waitFor("the notice", func() bool { return len(eventsOf(r, contract.TypeAlarmArmingScheduled)) == 1 })
	r.order(contract.CmdPostpone, contract.CommandArgs{}, contract.OriginApp, "u_owner")
	r.waitFor("the postponement", func() bool { return len(eventsOf(r, contract.TypeAlarmPostponed)) == 1 })
	r.clock.Advance(40 * time.Minute) // 22:30
	r.waitFor("the arming", func() bool { return len(eventsOf(r, contract.TypeAlarmArming)) == 1 })
	if d := dataOf[contract.ArmingData](t, eventsOf(r, contract.TypeAlarmArming)[0]); d.Origin != contract.OriginSchedule {
		t.Fatalf("%+v", d)
	}
	r.settle()
	st := r.broker.on(contract.AlarmStateTopic(site))
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
	st := r.broker.on(contract.AlarmStateTopic(site))
	d := dataOf[contract.AlarmStateData](t, st[len(st)-1])
	if d.NextScheduled == nil || !d.NextScheduled.Equal(t0.Add(time.Hour)) {
		t.Fatalf("%+v", d)
	}
	r.validateAll()
}

// --- health ------------------------------------------------------------------------

func TestHubBatteryAlertsAndHealth(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.device(devHub, contract.CapBattery, "100")
	r.device(devHub, contract.CapMains, "lost")
	r.device(devHub, contract.CapBattery, "38")
	r.settle()

	alerts := eventsOf(r, contract.TypeAlertRequest)
	if len(alerts) != 1 {
		t.Fatalf("alerts: %d", len(alerts))
	}
	a := dataOf[contract.AlertRequestData](t, alerts[0])
	if a.Kind != contract.AlertBatteryLowHub || a.Urgency != contract.UrgencyHigh || a.Percent != 38 || alerts[0].Env.Src != "escalation" {
		t.Fatalf("%+v", a)
	}
	h := r.broker.on(contract.HealthTopic(site))
	last := dataOf[contract.HealthData](t, h[len(h)-1])
	if last.Mains != contract.ValLost || last.HubBattery == nil || *last.HubBattery != 38 || !h[len(h)-1].Retain {
		t.Fatalf("%+v", last)
	}

	r.device(devHub, contract.CapMains, "present")
	r.settle()
	restored := false
	for _, al := range eventsOf(r, contract.TypeAlertRequest) {
		if dataOf[contract.AlertRequestData](t, al).Kind == contract.AlertMainsRestored {
			restored = true
		}
	}
	if !restored {
		t.Fatal("the return of the power must be announced after a battery alert")
	}
	r.validateAll()
}

func TestDailySummaryOfFaultsFromTheClock(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.device(devBack, contract.CapAvailability, "offline")
	r.device(devMotion, contract.CapBattery, "9")
	r.settle()
	r.clock.Advance(13 * time.Hour) // next morning, 10:00 (the summary is at 09:00)
	r.waitFor("the summary", func() bool { return len(eventsOf(r, contract.TypeAlertRequest)) == 2 })
	kinds := map[string][]string{}
	for _, al := range eventsOf(r, contract.TypeAlertRequest) {
		d := dataOf[contract.AlertRequestData](t, al)
		kinds[d.Kind] = d.Devices
		if d.Urgency != contract.UrgencyLow {
			t.Errorf("%+v", d)
		}
	}
	if len(kinds[contract.AlertDeviceOffline]) != 1 || kinds[contract.AlertDeviceOffline][0] != devBack ||
		len(kinds[contract.AlertBatteryLow]) != 1 || kinds[contract.AlertBatteryLow][0] != devMotion {
		t.Fatalf("%v", kinds)
	}
	r.validateAll()
}

func TestTheAgentStatusGivesTheInternetLink(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	internet := func() string {
		h := r.broker.on(contract.HealthTopic(site))
		return dataOf[contract.HealthData](t, h[len(h)-1]).Links["internet"]
	}
	r.settle()
	if got := internet(); got != "" {
		t.Fatalf("internet %q before the agent spoke, want none", got)
	}
	agentStatus(r, contract.UplinkUp)
	r.waitFor("internet ok", func() bool { r.settle(); return internet() == "ok" })
	agentStatus(r, contract.UplinkDown)
	r.waitFor("internet down", func() bool { r.settle(); return internet() == "down" })
	agentStatus(r, contract.UplinkUp)
	r.waitFor("internet ok again", func() bool { r.settle(); return internet() == "ok" })
	// The agent stops: after three minutes of silence, the internet counts as down.
	r.clock.Advance(3 * time.Minute)
	r.waitFor("internet silent", func() bool { r.settle(); return internet() == "silent" })
	r.validateAll()
}

// --- boot and restart ---------------------------------------------------------------

func TestBootLearnsTheDevicesFromRetainedStates(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.BootWindow = 50 * time.Millisecond })
	// The retained states arrive before Run's window ends: the back door was left open.
	state := func(id, capability, value string) {
		r.send(contract.DeviceStateTopic(site, id, capability), contract.TypeDeviceState, event(id, capability, value))
	}
	state(devBack, contract.CapContact, "open")
	state(devDoor, contract.CapContact, "closed")
	state(devHub, contract.CapBattery, "88")
	r.start()
	r.settle()

	var open map[string]bool
	r.hub.engLoop.Do(context.Background(), func(time.Time) []engine.Action { open = r.hub.eng.Snapshot().Open; return nil })
	if !open[devBack] || open[devDoor] {
		t.Fatalf("open doors after boot: %v", open)
	}
	// The engine now refuses to arm with the back door open.
	r.keypad(contract.ValArm, "")
	r.settle()
	if got := lastAck(t, r); got.Status != contract.AckRefused || got.Reason != contract.ReasonZoneOpen {
		t.Fatalf("%+v", got)
	}
	// And the health knows the battery, learned from a state too.
	h := r.broker.on(contract.HealthTopic(site))
	if d := dataOf[contract.HealthData](t, h[len(h)-1]); d.HubBattery == nil || *d.HubBattery != 88 {
		t.Fatalf("%+v", d)
	}
}

func TestBootWaitsForTheLinkBeforeItsWindowStarts(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.BootWindow = 20 * time.Millisecond; c.LinkWait = 10 * time.Second })
	r.startWithoutWaiting()
	time.Sleep(150 * time.Millisecond) // far longer than the window
	if len(r.broker.on(contract.AlarmStateTopic(site))) != 0 {
		t.Fatal("the hub booted before the link was up")
	}
	// The retained state arrives only now, after the window would have ended.
	r.send(contract.DeviceStateTopic(site, devBack, contract.CapContact), contract.TypeDeviceState, event(devBack, contract.CapContact, "open"))
	r.hub.LinkUp()
	r.hub.LinkUp() // harmless twice
	r.settle()
	r.waitFor("the first state", func() bool { return len(r.broker.on(contract.AlarmStateTopic(site))) > 0 })
}

func TestBootGivesUpWaitingForTheLink(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.BootWindow = 10 * time.Millisecond; c.LinkWait = 80 * time.Millisecond })
	r.startWithoutWaiting()
	r.waitFor("the boot without a link", func() bool { return len(r.broker.on(contract.AlarmStateTopic(site))) > 0 })
}

func TestStatesAfterBootAreIgnored(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.send(contract.DeviceStateTopic(site, devBack, contract.CapContact), contract.TypeDeviceState, event(devBack, contract.CapContact, "open"))
	r.settle()
	var open map[string]bool
	r.hub.engLoop.Do(context.Background(), func(time.Time) []engine.Action { open = r.hub.eng.Snapshot().Open; return nil })
	if open[devBack] {
		t.Fatal("after boot only events carry changes")
	}
}

func TestARestartKeepsTheAlarmArmedAndTheCodes(t *testing.T) {
	first := newRig(t, nil)
	first.start()
	first.setCode("u_owner", "482915")
	first.keypad(contract.ValArm, "")
	first.keypad(contract.ValArmNow, "")
	first.mustBe(engine.ModeArmed)
	first.stop()

	second := newRig(t, first.store)
	second.clock.Advance(time.Hour)
	second.start()
	second.mustBe(engine.ModeArmed)
	// The code survived: the owner can disarm.
	second.keypad(contract.ValDisarm, "482915")
	second.waitFor("disarmed", func() bool { return len(eventsOf(second, contract.TypeAlarmDisarmed)) == 1 })
	second.mustBe(engine.ModeDisarmed)
}

func TestSequenceNumbersOnlyGoUpAcrossRestarts(t *testing.T) {
	first := newRig(t, nil)
	first.start()
	first.keypad(contract.ValArm, "")
	first.settle()
	first.stop()
	var last uint64
	prev := uint64(0)
	for _, m := range first.broker.all() {
		if m.Env.Seq <= prev {
			t.Fatalf("seq %d after %d", m.Env.Seq, prev)
		}
		prev = m.Env.Seq
		last = m.Env.Seq
	}
	if last == 0 {
		t.Fatal("nothing was published")
	}

	second := newRig(t, first.store)
	second.start()
	second.settle()
	for _, m := range second.broker.all() {
		if m.Env.Seq <= last {
			t.Fatalf("after a restart, seq %d goes back under %d", m.Env.Seq, last)
		}
	}
}

// --- the broker goes away ------------------------------------------------------------

func TestMessagesWaitInOrderWhileTheBrokerIsDown(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()
	r.broker.mu.Lock()
	r.broker.failures = 5
	r.broker.mu.Unlock()

	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.device(devBack, contract.CapContact, "open")
	r.settle() // returns once the outbox has emptied, retries included

	var seqs []uint64
	for _, m := range r.broker.all() {
		seqs = append(seqs, m.Env.Seq)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("messages out of order after the outage: %v", seqs)
		}
	}
	if len(eventsOf(r, contract.TypeAlarmTriggered)) != 1 || len(r.broker.on(contract.DeviceCmdTopic(site, devSiren))) != 1 {
		t.Fatal("nothing may be lost during an outage")
	}
	r.validateAll()
}

func TestTheAlarmDoesNotWaitForTheBroker(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()
	r.broker.mu.Lock()
	r.broker.failures = 1 << 30 // the broker stays down
	r.broker.mu.Unlock()

	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.device(devBack, contract.CapContact, "open")
	// The engine moves on regardless.
	deadline := time.Now().Add(3 * time.Second)
	for r.mode() != engine.ModeTriggered {
		if time.Now().After(deadline) {
			t.Fatal("the engine is stuck behind the broker")
		}
		time.Sleep(time.Millisecond)
	}
	if r.hub.out.Depth() == 0 {
		t.Fatal("the messages must be waiting in the outbox")
	}
}

func TestARetainedStateIsReplacedWhileItWaits(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()
	r.broker.mu.Lock()
	r.broker.failures = 1 << 30
	r.broker.mu.Unlock()
	for i := 0; i < 5; i++ {
		agentStatus(r, []string{contract.UplinkUp, contract.UplinkDown}[i%2])
	}
	r.settle2()
	// Five changes, but only the last retained health still waits (plus the one in flight).
	if d := r.hub.out.Depth(); d > 2 {
		t.Fatalf("%d messages wait, a retained topic must keep only its latest", d)
	}
}

// settle2 waits for the domains only: the outbox is stuck on purpose.
func (r *rig) settle2() {
	r.t.Helper()
	r.hub.monLoop.Do(context.Background(), func(time.Time) []health.Action { return nil })
}

func TestARefusedMessageIsDroppedAndTheNextOnesGoOut(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()
	r.broker.mu.Lock()
	r.broker.refuse = func(topic string) bool { return topic == contract.AlarmAckTopic(site) }
	r.broker.mu.Unlock()
	r.keypad(contract.ValArm, "")
	r.settle()
	if len(eventsOf(r, contract.TypeAlarmArming)) != 1 {
		t.Fatal("the arming event must still go out behind a refused ack")
	}
	if len(r.broker.on(contract.AlarmAckTopic(site))) != 0 {
		t.Fatal("the refused message must not appear")
	}
}

func TestOutboxDropsTheOldestWhenFull(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.OutboxSize = 3 })
	// Not started: nothing leaves, the queue fills.
	for i := 0; i < 10; i++ {
		r.hub.enqueue("engine", contract.AlarmEventTopic(site), contract.TypeAlarmArmed, contract.ArmedData{Origin: "keypad"}, 1, false, t0)
	}
	if d := r.hub.out.Depth(); d != 3 {
		t.Fatalf("depth %d, want 3", d)
	}
	if !strings.Contains(r.logs.String(), "outbox full") {
		t.Fatal("dropping must be logged")
	}
}

// --- heartbeat -----------------------------------------------------------------------

func TestHeartbeatEveryMinute(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.clock.Advance(time.Minute)
	r.waitFor("the first heartbeat", func() bool { return len(eventsOf(r, contract.TypeHeartbeat)) == 1 })
	r.keypad(contract.ValArm, "")
	r.settle()
	r.clock.Advance(time.Minute)
	r.waitFor("the second heartbeat", func() bool { return len(eventsOf(r, contract.TypeHeartbeat)) == 2 })
	hbs := eventsOf(r, contract.TypeHeartbeat)
	a, b := dataOf[contract.HeartbeatData](t, hbs[0]), dataOf[contract.HeartbeatData](t, hbs[1])
	if a.BootID == "" || a.BootID != b.BootID || a.UptimeS != 60 || b.UptimeS != 120 || a.Engine != "disarmed" || b.Engine != "arming" ||
		a.FW["engine"] != "0.1.0" {
		t.Fatalf("%+v %+v", a, b)
	}
	if hbs[0].Retain {
		t.Fatal("a heartbeat is not retained")
	}
	r.settle()
	r.validateAll()
}

func TestTheHeartbeatSaysWhetherTheClockIsRight(t *testing.T) {
	for _, c := range []struct {
		name          string
		synced, known bool
		want          string
	}{
		{"synchronised", true, true, `"clock_ok":true`},
		{"not yet", false, true, `"clock_ok":false`},
		{"cannot tell", false, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, nil)
			r.hub.deps.ClockSynced = func() (bool, bool) { return c.synced, c.known }
			r.start()
			r.clock.Advance(time.Minute)
			r.waitFor("heartbeat", func() bool { return len(eventsOf(r, contract.TypeHeartbeat)) == 1 })
			data := string(eventsOf(r, contract.TypeHeartbeat)[0].Env.Data)
			if c.want == "" && strings.Contains(data, "clock_ok") || c.want != "" && !strings.Contains(data, c.want) {
				t.Fatalf("want %q in %s", c.want, data)
			}
			r.settle()
			r.validateAll()
		})
	}
}

func TestANewBootGetsANewBootID(t *testing.T) {
	first := newRig(t, nil)
	first.start()
	first.clock.Advance(time.Minute)
	first.waitFor("heartbeat", func() bool { return len(eventsOf(first, contract.TypeHeartbeat)) == 1 })
	first.stop()
	second := newRig(t, first.store)
	second.clock.Advance(time.Hour)
	second.start()
	second.clock.Advance(time.Minute)
	second.waitFor("heartbeat", func() bool { return len(eventsOf(second, contract.TypeHeartbeat)) == 1 })
	if dataOf[contract.HeartbeatData](t, eventsOf(first, contract.TypeHeartbeat)[0]).BootID ==
		dataOf[contract.HeartbeatData](t, eventsOf(second, contract.TypeHeartbeat)[0]).BootID {
		t.Fatal("a restart is a new boot: the cloud tells restarts apart from silence by it")
	}
}

// --- construction and shutdown ----------------------------------------------------------

func TestConfigValidation(t *testing.T) {
	if err := testConfig().validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Config){
		"bad site":        func(c *Config) { c.Site = "Lome" },
		"no hub device":   func(c *Config) { c.HubDeviceID = "" },
		"no siren":        func(c *Config) { c.SirenIDs = nil },
		"empty siren id":  func(c *Config) { c.SirenIDs = []string{""} },
		"no heartbeat":    func(c *Config) { c.HeartbeatEvery = 0 },
		"no siren ttl":    func(c *Config) { c.SirenCommandTTL = 0 },
		"retry inverted":  func(c *Config) { c.RetryMax = c.RetryMin / 2 },
		"negative window": func(c *Config) { c.BootWindow = -1 },
		"negative wait":   func(c *Config) { c.LinkWait = -1 },
		"no dedupe":       func(c *Config) { c.DedupeSize = 0 },
		"no outbox":       func(c *Config) { c.OutboxSize = 0 },
		"no auth queue":   func(c *Config) { c.AuthQueue = 0 },
	}
	for name, mutate := range bad {
		c := testConfig()
		mutate(&c)
		if _, err := New(c, Deps{Clock: newFakeClock(), Store: newMemStore(), Publisher: &fakeBroker{}, NewID: func() string { return "x" }}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := New(testConfig(), Deps{}); err == nil {
		t.Error("the ports are required")
	}
}

func TestNewRefusesADamagedSavedState(t *testing.T) {
	for _, key := range []string{keyEngine, keyHealth, keyAccess, keySeq} {
		store := newMemStore()
		store.docs[key] = []byte(`{"broken": [`)
		clock := newFakeClock()
		_, err := New(testConfig(), Deps{Clock: clock, Store: store, Publisher: &fakeBroker{}, NewID: contract.NewIDGeneratorWith(clock.Now, &entropy{}).New})
		if err == nil {
			t.Errorf("a damaged %s must stop the start", key)
		}
	}
}

func TestRunTwiceFailsAndShutdownFlushes(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	if err := r.hub.Run(context.Background()); err == nil {
		t.Fatal("Run twice must fail")
	}
	r.keypad(contract.ValArm, "")
	r.settle()
	r.stop()
	if len(eventsOf(r, contract.TypeAlarmArming)) != 1 {
		t.Fatal("the last events must leave before shutdown")
	}
	// After shutdown the hub gives up quietly.
	r.hub.HandleMessage(Message{Topic: contract.AlarmCmdTopic(site), Payload: mustEnv(t, r, contract.TypeAlarmCommand,
		contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp})})
}

func TestFloodOfCodeChecksDoesNotStallTheEngine(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.AuthQueue = 2 })
	r.start()
	r.setCode("u_owner", "482915")
	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)
	for i := 0; i < 50; i++ {
		r.keypad(contract.ValDisarm, fmt.Sprintf("%06d", 100000+i*7))
	}
	r.settle()
	r.mustBe(engine.ModeArmed)
	if len(eventsOf(r, contract.TypeAlarmAuthFailed)) == 0 {
		t.Fatal("the flood must be counted as wrong codes")
	}
}

func TestSubscriptions(t *testing.T) {
	r := newRig(t, nil)
	want := map[string]bool{
		"safehome/" + site + "/device/+/event":   true,
		"safehome/" + site + "/device/+/state/+": true,
		"safehome/" + site + "/alarm/cmd":        true,
		"safehome/" + site + "/alert/result":     true,
		"safehome/" + site + "/alert/ack":        true,
		"safehome/" + site + "/gsm/status":       true,
		"safehome/" + site + "/zigbee/status":    true,
		"safehome/" + site + "/agent/status":     true,
		"safehome/" + site + "/install/cmd":      true,
	}
	for _, s := range r.hub.Subscriptions() {
		if !want[s.Filter] || s.QoS != 1 {
			t.Errorf("unexpected subscription %+v", s)
		}
		delete(want, s.Filter)
	}
	if len(want) != 0 {
		t.Errorf("missing subscriptions: %v", want)
	}
}

func TestAccessRegistryIsSharedWithTheInstaller(t *testing.T) {
	r := newRig(t, nil)
	if err := r.hub.Codes().Set("u_owner", "482915"); err != nil {
		t.Fatal(err)
	}
	data, found, _ := r.store.Load(keyAccess)
	if !found {
		t.Fatal("a code must be saved as soon as it is set")
	}
	var recs []access.Record
	if err := json.Unmarshal(data, &recs); err != nil || len(recs) != 1 {
		t.Fatalf("%v %s", err, data)
	}
	if strings.Contains(string(data), "482915") {
		t.Fatal("the code must never be written in clear")
	}
}

// A device may not speak for another: the topic says who is talking, and the
// content must agree, or a compromised device could trip a zone that is not its own.
func TestADeviceMessageForAnotherDeviceIsIgnored(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)
	// "dev_motion" publishes on its own topic a message that claims to be the back door.
	r.send(contract.DeviceEventTopic(site, devMotion), contract.TypeDeviceEvent, event(devBack, contract.CapContact, "open"))
	r.settle()
	r.mustBe(engine.ModeArmed)
	if len(eventsOf(r, contract.TypeAlarmTriggered)) != 0 {
		t.Fatal("the zone of another device must not be tripped")
	}
	// The same message on the right topic does trip it.
	r.device(devBack, contract.CapContact, "open")
	r.mustBe(engine.ModeTriggered)
}

func TestReloadCodesPicksUpWhatAnotherProcessSaved(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)

	// The installer's tool, in another process, opens the same saved codes and adds one.
	cfg := testConfig()
	tool, err := OpenCodes(cfg.Access, r.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := tool.Set("u_new", "846029"); err != nil {
		t.Fatal(err)
	}

	// The running hub does not know it yet...
	acks := len(r.broker.ofType(contract.TypeAlarmAck))
	r.keypad(contract.ValDisarm, "846029")
	r.waitAcks(acks + 1)
	r.mustBe(engine.ModeArmed)

	// ...until it is told to look again.
	if err := r.hub.ReloadCodes(); err != nil {
		t.Fatal(err)
	}
	r.keypad(contract.ValDisarm, "846029")
	r.waitFor("disarmed", func() bool { return len(eventsOf(r, contract.TypeAlarmDisarmed)) == 1 })
	r.mustBe(engine.ModeDisarmed)

	// A damaged file is reported and leaves the codes in use alone.
	r.store.Save(keyAccess, []byte(`{"broken": [`))
	if err := r.hub.ReloadCodes(); err == nil {
		t.Fatal("a damaged file must be reported")
	}
	if len(r.hub.Codes().Users()) != 1 {
		t.Fatal("the codes in use must survive a failed reload")
	}
}

func TestEachCapabilityKeepsItsOwnRetainedState(t *testing.T) {
	// Two retained states of one device: the battery must not hide the open door.
	r := newRig(t, nil)
	r.send(contract.DeviceStateTopic(site, devBack, contract.CapContact), contract.TypeDeviceState, event(devBack, contract.CapContact, "open"))
	r.send(contract.DeviceStateTopic(site, devBack, contract.CapBattery), contract.TypeDeviceState, event(devBack, contract.CapBattery, "80"))
	// A state on the topic of another capability is refused.
	r.send(contract.DeviceStateTopic(site, devDoor, contract.CapBattery), contract.TypeDeviceState, event(devDoor, contract.CapContact, "open"))
	r.start()
	r.waitFor("the first state", func() bool { return len(r.broker.on(contract.AlarmStateTopic(site))) > 0 })
	r.order(contract.CmdArm, contract.CommandArgs{}, contract.OriginApp, "u_owner")
	r.waitFor("the reply", func() bool { return len(eventsOf(r, contract.TypeAlarmAck)) == 1 })
	ack := dataOf[contract.AlarmAckData](t, eventsOf(r, contract.TypeAlarmAck)[0])
	devs, _ := ack.Detail["devices"].([]any)
	if ack.Status != contract.AckRefused || len(devs) != 1 || devs[0] != devBack {
		t.Fatalf("only the back door is open: %+v", ack)
	}
}
