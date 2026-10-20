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
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/outbox"
	"safehome/internal/zigbee"
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
	if c.Base == "" || strings.ContainsAny(c.Base, "#+") || strings.HasSuffix(c.Base, "/") || overlapsContract(c.Base) {
		return fmt.Errorf("zigbeeapp: invalid base topic %q", c.Base)
	}
	if c.DedupeSize < 1 || c.OutboxSize < 1 || c.RetryMin <= 0 || c.RetryMax < c.RetryMin || c.StatusEvery <= 0 {
		return errors.New("zigbeeapp: DedupeSize, OutboxSize, RetryMin, RetryMax and StatusEvery must be set")
	}
	return c.Zigbee.Validate()
}

// overlapsContract tells whether a base topic would take in the contract's own
// topics: the adapter would read its own messages as Zigbee devices.
func overlapsContract(base string) bool {
	root, _, _ := strings.Cut(contract.Topic("x"), "/")
	return base == root || strings.HasPrefix(base, root+"/")
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
		a.repeatStatus(now)
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
				a.repeatStatus(now)
				return nil
			})
		}
	}
}
