package zigbeeapp

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/inbox"
	"safehome/internal/schematest"
	"safehome/internal/zigbee"
)

const site = "tg-lom-0001"

var t0 = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

// --- fakes ---------------------------------------------------------------------

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
	clock  *drivertest.FakeClock
	store  *drivertest.MemStore
	broker *fakeBroker
	logs   *drivertest.SyncBuffer
	ids    *contract.IDGenerator
	devIDs chan string
	cancel context.CancelFunc
	done   chan struct{}
}

func newRig(t *testing.T, store *drivertest.MemStore, mutate ...func(*Config)) *rig {
	t.Helper()
	if store == nil {
		store = drivertest.NewMemStore()
	}
	r := &rig{
		t: t, clock: drivertest.NewFakeClock(t0), store: store, broker: &fakeBroker{},
		logs: drivertest.Logs(t), ids: contract.NewIDGenerator(), devIDs: make(chan string, 10),
	}
	cfg := DefaultConfig(site, zigbee.DefaultConfig(profiles()))
	cfg.Outbox.RetryMin, cfg.Outbox.RetryMax = time.Millisecond, 5*time.Millisecond
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
	drivertest.WaitFor(r.t, "the first status", func() bool { return len(r.broker.ofType(contract.TypeZigbeeStatus)) > 0 })
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

// settle waits until the loop has handled everything handed to it so far.
func (r *rig) settle() {
	r.t.Helper()
	r.app.loop.Do(context.Background(), func(time.Time) []zigbee.Action { return nil })
	drivertest.WaitFor(r.t, "the outbox to empty", func() bool { return r.app.out.Depth() == 0 })
}

// z2m delivers a Zigbee2MQTT message.
func (r *rig) z2m(topic, payload string, retain bool) {
	r.app.HandleMessage(inbox.Message{Topic: "zigbee2mqtt/" + topic, Payload: []byte(payload), Retain: retain})
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
	r.app.HandleMessage(inbox.Message{Topic: topic, Payload: b})
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
	drivertest.WaitFor(r.t, id+" added", func() bool {
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
