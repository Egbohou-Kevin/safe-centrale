package hub

import (
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
	"safehome/internal/driver/drivertest"
	"safehome/internal/engine"
	"safehome/internal/escalation"
	"safehome/internal/health"
	"safehome/internal/inbox"
	"safehome/internal/outbox"
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
		return outbox.ErrUnavailable
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
	clock  *drivertest.FakeClock
	store  *drivertest.MemStore
	broker *fakeBroker
	logs   *drivertest.SyncBuffer
	ids    *contract.IDGenerator
	cancel context.CancelFunc
	done   chan struct{}
	seq    int
}

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
	c.Outbox.RetryMin, c.Outbox.RetryMax = time.Millisecond, 5*time.Millisecond
	c.Versions = map[string]string{"engine": "0.1.0"}
	return c
}

func newRig(t *testing.T, store *drivertest.MemStore, mutate ...func(*Config)) *rig {
	t.Helper()
	r := &rig{t: t, clock: drivertest.NewFakeClock(t0), store: store, broker: &fakeBroker{}, logs: drivertest.Logs(t)}
	if store == nil {
		r.store = drivertest.NewMemStore()
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

// start runs the hub and waits for its boot, letting the boot window pass on the
// fake clock (the rig has no LinkWait: the window starts at once).
func (r *rig) start() {
	r.startWithoutWaiting()
	if w := r.hub.cfg.BootWindow; w > 0 {
		r.clock.PassTimer(r.t, w)
	}
	drivertest.WaitFor(r.t, "boot", func() bool { return len(r.broker.on(contract.AlarmStateTopic(site))) >= 1 })
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

// settle waits until the domains have processed what was queued and the outbox is empty.
func (r *rig) settle() {
	r.t.Helper()
	ctx := context.Background()
	if err := r.hub.alarm.loop.Do(ctx, func(time.Time) []engine.Action { return nil }); err != nil {
		r.t.Fatal(err)
	}
	if err := r.hub.monitor.loop.Do(ctx, func(time.Time) []health.Action { return nil }); err != nil {
		r.t.Fatal(err)
	}
	// The escalation is fed by the two loops above: it comes last.
	if err := r.hub.escal.loop.Do(ctx, func(time.Time) []escalation.Action { return nil }); err != nil {
		r.t.Fatal(err)
	}
	drivertest.WaitFor(r.t, "the outbox to empty", func() bool { return r.hub.out.Depth() == 0 })
}

// send gives the hub a message as the broker would.
func (r *rig) send(topic, typ string, data any) string {
	r.t.Helper()
	return r.sendAt(r.clock.Now(), topic, typ, data)
}

// sendAt is send with a message written at ts: one the broker held, or from a
// clock that disagrees.
func (r *rig) sendAt(ts time.Time, topic, typ string, data any) string {
	r.t.Helper()
	r.seq++
	id := r.ids.New()
	env, err := contract.NewEnvelope(site, "test", uint64(r.seq), id, ts, typ, data)
	if err != nil {
		r.t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	r.hub.HandleMessage(inbox.Message{Topic: topic, Payload: b})
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
	r.keypadAt(r.clock.Now(), value, code)
}

// keypadAt is a key press written at ts.
func (r *rig) keypadAt(ts time.Time, value, code string) {
	r.t.Helper()
	d := event(devKeypad, contract.CapKeypad, value)
	d.Code = code
	r.sendAt(ts, contract.DeviceEventTopic(site, devKeypad), contract.TypeDeviceEvent, d)
}

func (r *rig) order(cmd string, args contract.CommandArgs, origin, user string) string {
	r.t.Helper()
	return r.send(contract.AlarmCmdTopic(site), contract.TypeAlarmCommand,
		contract.AlarmCommandData{Command: cmd, Args: args, Origin: origin, User: user})
}

// setCode sets a code as the installer's tools do: in the saved codes, then the hub
// is told to read them again.
func (r *rig) setCode(user, code string) {
	r.t.Helper()
	reg, err := openCodes(testConfig().Access, r.store)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := reg.Set(user, code); err != nil {
		r.t.Fatal(err)
	}
	if err := r.hub.ReloadCodes(); err != nil {
		r.t.Fatal(err)
	}
}

// mode reads the engine's mode from the loop's goroutine.
func (r *rig) mode() (m engine.Mode) {
	r.t.Helper()
	if err := r.hub.alarm.loop.Do(context.Background(), func(time.Time) []engine.Action { m = r.hub.alarm.eng.Mode(); return nil }); err != nil {
		r.t.Fatal(err)
	}
	return m
}

// mustBe waits for the engine to reach the mode: ticks and code checks are handled
// by goroutines of their own, so the mode may take a moment to settle.
func (r *rig) mustBe(m engine.Mode) {
	r.t.Helper()
	drivertest.WaitFor(r.t, "mode "+string(m), func() bool { r.settle(); return r.mode() == m })
}

// waitAcks waits until n acknowledgements have been published.
func (r *rig) waitAcks(n int) {
	r.t.Helper()
	drivertest.WaitFor(r.t, fmt.Sprintf("%d acks", n), func() bool { return len(r.broker.ofType(contract.TypeAlarmAck)) >= n })
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
