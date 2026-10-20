// Package zigbeeapp is the application layer of the Zigbee adapter: it feeds the
// zigbee core with what Zigbee2MQTT publishes and with the orders of the contract
// (device commands from the engine, pairing and removal from the installer), and
// publishes what the core decides: device events and states, command
// acknowledgements, the registry, and the requests to Zigbee2MQTT.
//
// Zigbee2MQTT shares the site's broker, so the adapter needs no port of its own
// besides the broker: the composition root (cmd/safehome-zigbee) wires the MQTT
// client to it.
package zigbeeapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/outbox"
	"safehome/internal/zigbee"
	"safehome/internal/zigbee/wire"
	"safehome/internal/zigbee/z2m"
)

// Config holds what the adapter needs to know.
type Config struct {
	Site       string
	Base       string // Zigbee2MQTT's base topic
	Zigbee     zigbee.Config
	DedupeSize int
	OutboxSize int
	RetryMin   time.Duration
	RetryMax   time.Duration
	// StatusEvery is how often zigbee.status goes out even when nothing changed: the
	// hub takes an adapter that stopped repeating it for dead.
	StatusEvery time.Duration
}

// DefaultConfig returns the defaults.
func DefaultConfig(site string, z zigbee.Config) Config {
	return Config{
		Site: site, Base: z2m.DefaultBase, Zigbee: z, DedupeSize: 1024, OutboxSize: 1000,
		RetryMin: 200 * time.Millisecond, RetryMax: 5 * time.Second, StatusEvery: time.Minute,
	}
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if !contract.ValidSite(c.Site) {
		return fmt.Errorf("zigbeeapp: invalid site %q", c.Site)
	}
	if c.Base == "" || strings.ContainsAny(c.Base, "#+") || strings.HasSuffix(c.Base, "/") {
		return fmt.Errorf("zigbeeapp: invalid base topic %q", c.Base)
	}
	if c.DedupeSize < 1 || c.OutboxSize < 1 || c.RetryMin <= 0 || c.RetryMax < c.RetryMin || c.StatusEvery <= 0 {
		return errors.New("zigbeeapp: DedupeSize, OutboxSize, RetryMin, RetryMax and StatusEvery must be set")
	}
	return c.Zigbee.Validate()
}

// Deps are the ports the adapter needs.
type Deps struct {
	Clock     driver.Clock
	Store     driver.Store
	Publisher outbox.Publisher
	NewID     func() string // message ids
	// NewDeviceID proposes ids for new devices, for example "dev_k7p2".
	NewDeviceID func() string
	Logger      *slog.Logger
}

// Message is a message received from the broker.
type Message struct {
	Topic   string
	Payload []byte
	Retain  bool
}

// Subscription is a topic filter the adapter listens to.
type Subscription struct {
	Filter string
	QoS    byte
}

const (
	src        = contract.AdapterZigbee
	keyZigbee  = "zigbee"
	keySeq     = "zigbee_seq"
	maxPayload = 64 * 1024
)

// App is the application.
type App struct {
	cfg  Config
	deps Deps
	log  *slog.Logger

	core *zigbee.Network
	loop *driver.Loop[zigbee.Input, zigbee.Action, zigbee.Snapshot]
	out  *outbox.Outbox
	seen *outbox.RecentIDs

	last contract.ZigbeeStatusData // the last status published, owned by the loop
	sent bool

	life context.Context
	stop context.CancelFunc
	ran  atomic.Bool
}

// New builds the adapter, loading the saved registry and states.
func New(cfg Config, deps Deps) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.Clock == nil || deps.Store == nil || deps.Publisher == nil || deps.NewID == nil || deps.NewDeviceID == nil {
		return nil, errors.New("zigbeeapp: Clock, Store, Publisher, NewID and NewDeviceID are required")
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	a := &App{cfg: cfg, deps: deps, log: log, seen: outbox.NewRecentIDs(cfg.DedupeSize)}
	a.life, a.stop = context.WithCancel(context.Background())

	var err error
	if a.core, err = zigbee.New(cfg.Zigbee, deps.NewDeviceID); err != nil {
		return nil, err
	}
	seq, err := outbox.NewSequencer(deps.Store, keySeq, log)
	if err != nil {
		return nil, err
	}
	a.out = outbox.New(deps.Publisher, seq, cfg.Site, cfg.OutboxSize, cfg.RetryMin, cfg.RetryMax, log)
	a.loop, err = driver.New(driver.Options[zigbee.Input, zigbee.Action, zigbee.Snapshot]{
		Name: keyZigbee, Clock: deps.Clock, Store: deps.Store, Logger: log,
		Core: driver.Core[zigbee.Input, zigbee.Action, zigbee.Snapshot]{
			Handle: a.core.Handle, NextWake: a.core.NextWake, Snapshot: a.core.Snapshot, Restore: a.core.Restore,
			Tick: func(t time.Time) zigbee.Input { return zigbee.Tick{Now: t} },
		},
		Dispatch: a.dispatch,
		OnChange: func(_ context.Context, now time.Time) { a.publishStatus(now) },
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// Subscriptions lists what the adapter listens to.
func (a *App) Subscriptions() []Subscription {
	return []Subscription{
		{Filter: z2m.Filter(a.cfg.Base), QoS: 1},
		{Filter: contract.Topic(a.cfg.Site, "device", "+", "cmd"), QoS: 1},
		{Filter: contract.RegistryCmdTopic(a.cfg.Site), QoS: 1},
	}
}

// Alive returns once the core's loop has taken what was queued for it: it is not
// stuck. The program checks it before telling systemd that it still works (the
// watchdog).
func (a *App) Alive(ctx context.Context) error { return a.loop.Alive(ctx) }

// Run serves the adapter until ctx is cancelled. It must be called once.
func (a *App) Run(ctx context.Context) error {
	if !a.ran.CompareAndSwap(false, true) {
		return errors.New("zigbeeapp: Run called twice")
	}
	defer a.stop()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{}, 3)
	start := func(f func(context.Context)) { go func() { f(runCtx); done <- struct{}{} }() }
	start(func(c context.Context) { a.loop.Run(c) })
	start(a.out.Run)
	// The registry and the status, even if nothing changed since the last run.
	a.loop.Do(runCtx, func(now time.Time) []zigbee.Action {
		a.publishRegistry(now, a.core.Devices())
		a.sent = false
		a.publishStatus(now)
		return nil
	})
	start(a.statusRepeater)
	<-runCtx.Done()
	for range 3 {
		<-done
	}
	a.out.Flush(3 * time.Second)
	return nil
}

// statusRepeater sends the status again every StatusEvery, through the core's loop:
// a loop that hangs stops the repetition, and the hub notices.
func (a *App) statusRepeater(ctx context.Context) {
	for {
		fired, stop := a.deps.Clock.Timer(a.cfg.StatusEvery)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-fired:
			a.loop.Do(ctx, func(now time.Time) []zigbee.Action {
				a.sent = false
				a.publishStatus(now)
				return nil
			})
		}
	}
}

// --- inbound -------------------------------------------------------------------

// HandleMessage takes a message from the broker. It returns at once: the work is
// queued on the core's loop.
func (a *App) HandleMessage(m Message) {
	if len(m.Payload) > maxPayload {
		a.log.Warn("ignoring an oversized message", "topic", m.Topic, "bytes", len(m.Payload))
		return
	}
	if strings.HasPrefix(m.Topic, a.cfg.Base+"/") {
		a.fromBridge(m)
		return
	}
	site, parts, ok := contract.ParseTopic(m.Topic)
	if !ok || site != a.cfg.Site {
		return
	}
	switch {
	case len(parts) == 3 && parts[0] == "device" && parts[2] == "cmd":
		a.deviceCommand(parts[1], m)
	case len(parts) == 2 && parts[0] == "registry" && parts[1] == "cmd":
		a.registryOrder(m)
	}
}

func (a *App) fromBridge(m Message) {
	if req, reason, failed := z2m.ResponseError(a.cfg.Base, m.Topic, m.Payload); failed {
		a.log.Error("Zigbee2MQTT refused a request", "request", req, "error", reason)
		return
	}
	if _, ok, err := z2m.Parse(a.cfg.Base, m.Topic, m.Payload, m.Retain, time.Time{}); err != nil || !ok {
		if err != nil {
			a.log.Warn("ignoring a message from Zigbee2MQTT", "topic", m.Topic, "err", err)
		}
		return
	}
	a.submit(func(now time.Time) zigbee.Input {
		in, _, _ := z2m.Parse(a.cfg.Base, m.Topic, m.Payload, m.Retain, now)
		return in
	})
}

// envelope parses and checks the envelope of an order, and drops a duplicate.
func (a *App) envelope(m Message, types ...string) (contract.Envelope, bool) {
	env, err := contract.ParseEnvelope(m.Payload)
	if err != nil || env.Site != a.cfg.Site {
		a.log.Warn("ignoring an invalid order", "topic", m.Topic, "err", err)
		return contract.Envelope{}, false
	}
	known := false
	for _, t := range types {
		known = known || env.Type == t
	}
	if !known {
		a.log.Warn("ignoring a message of the wrong type for its topic", "topic", m.Topic, "type", env.Type)
		return contract.Envelope{}, false
	}
	return env, a.seen.FirstTime(env.ID)
}

func (a *App) deviceCommand(deviceID string, m Message) {
	env, ok := a.envelope(m, contract.TypeCommand)
	if !ok {
		return
	}
	d, err := contract.DecodeData[contract.DeviceCommandData](env)
	if err == nil {
		err = d.Validate()
	}
	if err == nil && d.DeviceID != deviceID {
		err = fmt.Errorf("topic for %s, command for %s", deviceID, d.DeviceID)
	}
	if err != nil {
		a.log.Warn("ignoring an invalid device command", "topic", m.Topic, "err", err)
		return
	}
	a.submit(func(now time.Time) zigbee.Input { return wire.Command(d, env.ID, now) })
}

func (a *App) registryOrder(m Message) {
	env, ok := a.envelope(m, contract.TypeRegistryPair, contract.TypeRegistryRemove)
	if !ok {
		return
	}
	if env.Type == contract.TypeRegistryPair {
		d, err := contract.DecodeData[contract.RegistryPairData](env)
		if err == nil {
			err = d.Validate()
		}
		if err != nil {
			a.log.Warn("ignoring an invalid pairing order", "err", err)
			return
		}
		a.log.Info("pairing order", "seconds", d.DurationS)
		a.submit(func(now time.Time) zigbee.Input { return wire.Pair(d, env.ID, now) })
		return
	}
	d, err := contract.DecodeData[contract.RegistryRemoveData](env)
	if err == nil {
		err = d.Validate()
	}
	if err != nil {
		a.log.Warn("ignoring an invalid removal order", "err", err)
		return
	}
	a.log.Info("removal order", "device", d.DeviceID)
	a.submit(func(now time.Time) zigbee.Input { return wire.Remove(d, env.ID, now) })
}

func (a *App) submit(build func(time.Time) zigbee.Input) {
	if err := a.loop.Submit(a.life, build); err != nil && a.life.Err() == nil {
		a.log.Error("handing an input to the core failed", "err", err)
	}
}

// --- outbound ------------------------------------------------------------------

// dispatch runs on the loop's goroutine and only queues messages.
func (a *App) dispatch(_ context.Context, now time.Time, actions []zigbee.Action) error {
	site := a.cfg.Site
	for _, act := range actions {
		switch v := act.(type) {
		case zigbee.Changed:
			out, err := wire.Changed(v)
			if err != nil {
				a.log.Error("a value refused by the contract: check the profile", "device", v.DeviceID, "err", err)
				continue
			}
			a.publishDevice(now, out)
		case zigbee.Presence:
			a.log.Info("device presence", "device", v.DeviceID, "online", v.Online)
			a.publishDevice(now, wire.Presence(v))
		case zigbee.Ack:
			ack, err := wire.Ack(v)
			if err != nil {
				return err
			}
			if v.Status == zigbee.AckFailed || v.Status == zigbee.AckExpired {
				a.log.Warn("a device command did not succeed", "device", v.DeviceID, "status", v.Status, "reason", v.Reason)
			}
			a.enqueue(contract.DeviceAckTopic(site, v.DeviceID), contract.TypeCommandAck, ack, false, now)
		case zigbee.Added:
			a.log.Info("device added", "device", v.Device.ID, "profile", v.Device.Profile)
			a.enqueue(contract.RegistryEventTopic(site), contract.TypeRegistryDeviceAdded, wire.Added(v), false, now)
		case zigbee.Rejected:
			r, err := wire.Rejected(v)
			if err != nil {
				return err
			}
			a.log.Warn("device refused", "model", v.Model, "reason", v.Reason)
			a.enqueue(contract.RegistryEventTopic(site), contract.TypeRegistryDeviceRejected, r, false, now)
		case zigbee.Removed:
			a.log.Info("device removed", "device", v.Device.ID)
			a.enqueue(contract.RegistryEventTopic(site), contract.TypeRegistryDeviceRemoved, wire.Removed(v), false, now)
			for _, c := range wire.StateCapabilities(v.Device) {
				// An empty retained message clears the state: a removed door left
				// "open" would otherwise block every arming.
				a.out.Put(&outbox.Item{Topic: contract.DeviceStateTopic(site, v.Device.ID, c), QoS: 1, Retain: true, Raw: true})
			}
		case zigbee.RegistryChanged:
			a.publishRegistry(now, v.Devices)
		case zigbee.Ignored:
			a.log.Debug("ignored", "what", v.What)
		default:
			msg, ok, err := z2m.Build(a.cfg.Base, act)
			if err != nil {
				return err
			}
			if ok {
				a.out.Put(&outbox.Item{Topic: msg.Topic, Data: msg.Payload, QoS: 1, Raw: true})
			}
		}
	}
	return nil
}

func (a *App) publishDevice(now time.Time, d wire.Device) {
	if d.Event != nil {
		a.enqueue(contract.DeviceEventTopic(a.cfg.Site, d.Event.DeviceID), contract.TypeDeviceEvent, *d.Event, false, now)
	}
	if d.State != nil {
		a.enqueue(contract.DeviceStateTopic(a.cfg.Site, d.State.DeviceID, d.State.Capability), contract.TypeDeviceState, *d.State, true, now)
	}
}

func (a *App) publishRegistry(now time.Time, devices []zigbee.Device) {
	a.enqueue(contract.RegistryDevicesTopic(a.cfg.Site), contract.TypeRegistrySnapshot, wire.Snapshot(devices), true, now)
}

// publishStatus publishes zigbee.status, retained, when it changed (or when sent is
// cleared, to repeat it). It runs on the loop's goroutine.
func (a *App) publishStatus(now time.Time) {
	st := wire.Status(a.core.Status())
	if a.sent && reflect.DeepEqual(st, a.last) {
		return
	}
	a.last, a.sent = st, true
	a.enqueue(contract.ZigbeeStatusTopic(a.cfg.Site), contract.TypeZigbeeStatus, st, true, now)
}

func (a *App) enqueue(topic, typ string, data any, retain bool, ts time.Time) {
	raw, err := json.Marshal(data)
	if err != nil {
		a.log.Error("encoding a message failed", "type", typ, "err", err)
		return
	}
	a.out.Put(&outbox.Item{Topic: topic, Src: src, Type: typ, Data: raw, QoS: 1, Retain: retain, ID: a.deps.NewID(), TS: ts})
}
