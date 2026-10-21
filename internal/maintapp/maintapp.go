// Package maintapp is the application layer of remote maintenance: it feeds the
// maintenance core with the cloud's orders (maint/cmd, passed on by the link agent),
// opens and closes the tunnel it decides, and publishes maint.session, which the
// agent sends to the cloud and the owner sees.
//
// The tunnel is behind the Tunneler port, declared here and wired in the
// composition root (cmd/safehome-maint) to ssh.
package maintapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/inbox"
	"safehome/internal/maint"
	maintwire "safehome/internal/maint/wire"
	"safehome/internal/outbox"
)

// Tunneler opens the tunnel to the bastion.
type Tunneler interface {
	// Open starts the tunnel and returns at once; Tunnel says what becomes of it.
	Open(ctx context.Context) (Tunnel, error)
}

// Tunnel is a tunnel under way.
type Tunnel interface {
	Up() <-chan struct{}   // closed once the bastion accepted it
	Done() <-chan struct{} // closed once it stopped, whatever the cause
	Stop()
}

// Config holds what the service needs to know.
type Config struct {
	Site       string
	Maint      maint.Config
	Outbox     outbox.Settings
	DedupeSize int
}

// DefaultConfig returns the defaults.
func DefaultConfig(site string, m maint.Config) Config {
	return Config{Site: site, Maint: m, Outbox: outbox.DefaultSettings(50), DedupeSize: 64}
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if !contract.ValidSite(c.Site) {
		return fmt.Errorf("maintapp: invalid site %q", c.Site)
	}
	if c.DedupeSize < 1 {
		return errors.New("maintapp: DedupeSize must be set")
	}
	if err := c.Outbox.Validate(); err != nil {
		return fmt.Errorf("maintapp: %w", err)
	}
	return c.Maint.Validate()
}

// Deps are the ports the service needs.
type Deps struct {
	Clock     driver.Clock
	Store     driver.Store
	Publisher outbox.Publisher
	Tunneler  Tunneler
	NewID     func() string
	Logger    *slog.Logger
}

const (
	src        = "maint"
	keyMaint   = "maint"
	keySeq     = "maint_seq"
	maxPayload = 8 * 1024
)

// App is the application.
type App struct {
	cfg  Config
	deps Deps
	log  *slog.Logger

	core *maint.Service
	loop *driver.Loop[maint.Input, maint.Action, maint.Snapshot]
	out  *outbox.Outbox
	seen *inbox.RecentIDs

	// base is the tunnels' parent, set by Run before the loop starts; cancel stops
	// the tunnel under way. Both belong to the loop's goroutine.
	base    context.Context
	cancel  context.CancelFunc
	tunnels sync.WaitGroup

	life context.Context
	stop context.CancelFunc
	ran  atomic.Bool
}

// New builds the service, loading a session saved before a restart.
func New(cfg Config, deps Deps) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.Clock == nil || deps.Store == nil || deps.Publisher == nil || deps.Tunneler == nil || deps.NewID == nil {
		return nil, errors.New("maintapp: Clock, Store, Publisher, Tunneler and NewID are required")
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	a := &App{cfg: cfg, deps: deps, log: log, seen: inbox.NewRecentIDs(cfg.DedupeSize)}
	a.life, a.stop = context.WithCancel(context.Background())
	var err error
	if a.core, err = maint.New(cfg.Maint); err != nil {
		return nil, err
	}
	if a.out, err = outbox.Open(deps.Store, deps.Publisher, keySeq, cfg.Site, cfg.Outbox, log); err != nil {
		return nil, err
	}
	a.loop, err = driver.New(driver.Options[maint.Input, maint.Action, maint.Snapshot]{
		Name: keyMaint, Clock: deps.Clock, Store: deps.Store, Logger: log,
		Core: driver.Core[maint.Input, maint.Action, maint.Snapshot]{
			Handle: a.core.Handle, NextWake: a.core.NextWake, Snapshot: a.core.Snapshot, Restore: a.core.Restore,
			Tick: func(t time.Time) maint.Input { return maint.Tick{Now: t} },
		},
		Dispatch: a.dispatch,
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// Subscriptions lists what the service listens to.
func (a *App) Subscriptions() []inbox.Subscription {
	return []inbox.Subscription{{Filter: contract.MaintCmdTopic(a.cfg.Site), QoS: 1}}
}

// Alive returns once the core's loop has taken what was queued for it (the
// watchdog).
func (a *App) Alive(ctx context.Context) error { return a.loop.Alive(ctx) }

// Run serves the service until ctx is cancelled. It must be called once. At the end
// the tunnel is closed: a session saved goes on at the next start, until its time.
func (a *App) Run(ctx context.Context) error {
	if !a.ran.CompareAndSwap(false, true) {
		return errors.New("maintapp: Run called twice")
	}
	defer a.stop()
	a.base = ctx
	var wg sync.WaitGroup
	for _, run := range []func(context.Context){func(c context.Context) { a.loop.Run(c) }, a.out.Run} {
		wg.Go(func() { run(ctx) })
	}
	// An order received before this point is taken first; Boot leaves its session alone.
	a.loop.Do(ctx, func(now time.Time) []maint.Action { return a.core.Handle(maint.Boot{Now: now}) })
	wg.Wait()
	a.tunnels.Wait()
	a.out.Flush()
	return nil
}

// HandleMessage takes a message from the broker. It returns quickly.
func (a *App) HandleMessage(m inbox.Message) {
	if len(m.Payload) > maxPayload || m.Topic != contract.MaintCmdTopic(a.cfg.Site) {
		return
	}
	env, err := inbox.Envelope(m, a.cfg.Site, contract.TypeMaintOpen, contract.TypeMaintClose)
	if err != nil {
		a.log.Warn("ignoring an invalid maintenance order", "err", err)
		return
	}
	if !a.seen.FirstTime(env.ID) {
		return
	}
	switch env.Type {
	case contract.TypeMaintOpen:
		d, err := contract.ValidData[contract.MaintOpenData](env)
		if err != nil {
			a.log.Warn("ignoring an invalid maintenance order", "id", env.ID, "err", err)
			return
		}
		a.log.Info("remote maintenance asked", "session", d.Session, "user", d.User, "reason", d.Reason, "duration_s", d.DurationS)
		a.submit(func(now time.Time) maint.Input { return maintwire.Open(d, now) })
	case contract.TypeMaintClose:
		d, err := contract.ValidData[contract.MaintCloseData](env)
		if err != nil {
			a.log.Warn("ignoring an invalid maintenance order", "id", env.ID, "err", err)
			return
		}
		a.submit(func(now time.Time) maint.Input { return maintwire.Close(d, now) })
	}
}

func (a *App) submit(build func(time.Time) maint.Input) {
	if err := a.loop.Submit(a.life, build); err != nil && a.life.Err() == nil {
		a.log.Error("handing an input to the maintenance service failed", "err", err)
	}
}

// dispatch runs on the loop's goroutine.
func (a *App) dispatch(_ context.Context, now time.Time, actions []maint.Action) error {
	for _, act := range actions {
		switch v := act.(type) {
		case maint.Start:
			a.startTunnel(v.Session)
		case maint.Stop:
			a.stopTunnel()
		case maint.Report:
			a.log.Info("remote maintenance", "session", v.Session, "user", v.User, "status", v.Status, "reason", v.Reason,
				"until", v.Until, "open_for", v.Duration)
			a.enqueue(now, maintwire.Session(v))
		}
	}
	return nil
}

// startTunnel opens the tunnel away from the loop. It runs on the loop's goroutine.
func (a *App) startTunnel(session string) {
	if a.base == nil || a.base.Err() != nil {
		return
	}
	if a.cancel != nil {
		a.cancel()
	}
	ctx, cancel := context.WithCancel(a.base)
	a.cancel = cancel
	a.tunnels.Add(1)
	go func() {
		defer a.tunnels.Done()
		defer cancel()
		a.runTunnel(ctx, session)
	}()
}

// runTunnel opens the tunnel and tells the core when it is up and when it stopped
// by itself, until ctx closes it on purpose.
func (a *App) runTunnel(ctx context.Context, session string) {
	down := func() { a.submit(func(now time.Time) maint.Input { return maint.Down{Now: now, Session: session} }) }
	t, err := a.deps.Tunneler.Open(ctx)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warn("the maintenance tunnel could not start", "session", session, "err", err)
			down()
		}
		return
	}
	defer t.Stop()
	up := t.Up()
	sayUp := func() {
		up = nil
		a.submit(func(now time.Time) maint.Input { return maint.Up{Now: now, Session: session} })
	}
	for {
		select {
		case <-ctx.Done():
			return // closed on purpose
		case <-up:
			sayUp()
		case <-t.Done():
			if ctx.Err() != nil {
				return
			}
			// Up then down at once: up first, or the session would fail instead of
			// being opened again.
			select {
			case <-up:
				sayUp()
			default:
			}
			a.log.Warn("the maintenance tunnel stopped", "session", session)
			down()
			return
		}
	}
}

// stopTunnel closes the tunnel under way. It runs on the loop's goroutine.
func (a *App) stopTunnel() {
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
}

func (a *App) enqueue(now time.Time, d contract.MaintSessionData) {
	raw, err := json.Marshal(d)
	if err != nil {
		return
	}
	a.out.Put(&outbox.Item{Topic: contract.MaintSessionTopic(a.cfg.Site), Src: src, Type: contract.TypeMaintSession, Data: raw, QoS: 1, ID: a.deps.NewID(), TS: now})
}
