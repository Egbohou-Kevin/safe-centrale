package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// pending reports whether a timer waits to fire at when.
func (c *fakeClock) pending(when time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for t := range c.timers {
		if t.when.Equal(when) {
			return true
		}
	}
	return false
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
	hold     chan struct{} // set: every publish waits for it to close
}

func (b *fakeBroker) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	b.mu.Lock()
	hold := b.hold
	b.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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
