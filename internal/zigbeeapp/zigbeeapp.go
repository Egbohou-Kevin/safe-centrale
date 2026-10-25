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
	"sync"
	"sync/atomic"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/inbox"
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
	Outbox     outbox.Settings
	// StatusEvery is how often zigbee.status goes out even when nothing changed: the
	// hub takes an adapter that stopped repeating it for dead.
	StatusEvery time.Duration
}

// DefaultConfig returns the defaults.
func DefaultConfig(site string, z zigbee.Config) Config {
	return Config{
		Site: site, Base: z2m.DefaultBase, Zigbee: z, DedupeSize: 1024, Outbox: outbox.DefaultSettings(1000),
		StatusEvery: time.Minute,
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
	if c.DedupeSize < 1 || c.StatusEvery <= 0 {
		return errors.New("zigbeeapp: DedupeSize and StatusEvery must be set")
	}
	if err := c.Outbox.Validate(); err != nil {
		return fmt.Errorf("zigbeeapp: %w", err)
	}
	return c.Zigbee.Validate()
}

// overlapsContract tells whether a base topic would take in the contract's own
// topics: the adapter would read its own messages as Zigbee devices.
func overlapsContract(base string) bool {
	return base == contract.TopicRoot || strings.HasPrefix(base, contract.TopicRoot+"/")
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
	seen *inbox.RecentIDs

	last contract.ZigbeeStatusData // the last status published, owned by the loop
	sent bool

	// life ends with Run: the loop runs on it, and work handed to it after it ended
	// is given up quietly.
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
	a := &App{cfg: cfg, deps: deps, log: log, seen: inbox.NewRecentIDs(cfg.DedupeSize)}
	a.life, a.stop = context.WithCancel(context.Background())

	var err error
	if a.core, err = zigbee.New(cfg.Zigbee, deps.NewDeviceID); err != nil {
		return nil, err
	}
	if a.out, err = outbox.Open(deps.Store, deps.Publisher, keySeq, cfg.Site, cfg.Outbox, deps.NewID, log); err != nil {
		return nil, err
	}
	a.loop, err = driver.New(driver.Options[zigbee.Input, zigbee.Action, zigbee.Snapshot]{
		Name: keyZigbee, Clock: deps.Clock, Store: deps.Store, Logger: log,
		// Zigbee2MQTT's last_seen follows the system clock, set back too: so does the
		// core, which pulls back what that leaves in the future (see zigbee.pullBack).
		FollowsClock: true,
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
func (a *App) Subscriptions() []inbox.Subscription {
	var subs []inbox.Subscription
	for _, f := range z2m.Filters(a.cfg.Base) {
		subs = append(subs, inbox.Subscription{Filter: f, QoS: 1})
	}
	return append(subs,
		inbox.Subscription{Filter: contract.Topic(a.cfg.Site, "device", "+", "cmd"), QoS: 1},
		inbox.Subscription{Filter: contract.RegistryCmdTopic(a.cfg.Site), QoS: 1},
	)
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
	defer context.AfterFunc(ctx, a.stop)()
	// Everything runs on life, which ends with ctx: a loop only stops once life is
	// over, so a message handed to it meanwhile is known to be given up.
	var wg sync.WaitGroup
	for _, run := range []func(context.Context){
		func(c context.Context) { a.loop.Run(c) },
		a.out.Run,
		// The status again every StatusEvery, through the core's loop: a loop that
		// hangs stops the repetition, and the hub notices.
		func(c context.Context) { driver.Every(c, a.deps.Clock, a.cfg.StatusEvery, a.repeatStatus) },
	} {
		wg.Go(func() { run(a.life) })
	}
	// The registry and the status, even if nothing changed since the last run.
	a.loop.Do(a.life, func(now time.Time) []zigbee.Action {
		a.publishRegistry(now, a.core.Devices())
		a.publishStatusAgain(now)
		return nil
	})
	wg.Wait()
	a.closeNetwork()
	a.out.Flush()
	return nil
}

// closeNetwork closes a network left open for pairing as the adapter stops: a device
// joining while no adapter listens would stay in Zigbee2MQTT's network, outside the
// registry, unwatched. The window is kept: the next start opens it again for what is
// left. A window Zigbee2MQTT has not confirmed yet is closed too: its answer may
// only be late. It runs once the loop is over, so the core is no longer in use.
func (a *App) closeNetwork() {
	if a.core.Snapshot().PairUntil.IsZero() {
		return
	}
	a.log.Info("closing the network for pairing until the adapter is back")
	if err := a.toBridge(zigbee.PermitJoin{}); err != nil {
		a.log.Error("closing the network failed", "err", err)
	}
}

// repeatStatus publishes zigbee.status again, through the core's loop.
func (a *App) repeatStatus(ctx context.Context) {
	a.loop.Do(ctx, func(now time.Time) []zigbee.Action {
		a.publishStatusAgain(now)
		return nil
	})
}
