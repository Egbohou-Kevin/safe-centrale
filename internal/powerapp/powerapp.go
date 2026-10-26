// Package powerapp is the application layer of the hub's battery module: it reads the
// module every PollEvery, feeds the readings to the power core, publishes the mains
// and battery of the hub's own device (device.event and the retained device.state,
// as an adapter does for a sensor), and asks the module to cut the power when the
// core says so.
//
// The module is behind the Board port, declared here and wired in the composition
// root (cmd/safehome-power) to the X728.
package powerapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/outbox"
	"safehome/internal/power"
	powerwire "safehome/internal/power/wire"
)

// Board is the battery module.
type Board interface {
	MainsLost(ctx context.Context) (bool, error)
	Gauge(ctx context.Context) (milliVolts, percent int, err error)
	// Cut makes the module shut the hub down and cut its power. It returns once the
	// module was told.
	Cut(ctx context.Context) error
}

// Config holds what the service needs to know.
type Config struct {
	Site      string
	DeviceID  string // the hub's own device: hub_device_id in the hub's configuration
	Power     power.Config
	PollEvery time.Duration
	Outbox    outbox.Settings
}

// DefaultConfig returns the defaults.
func DefaultConfig(site, deviceID string, p power.Config) Config {
	return Config{Site: site, DeviceID: deviceID, Power: p, PollEvery: time.Second, Outbox: outbox.DefaultSettings(50)}
}

// Validate checks the configuration.
func (c Config) Validate() error {
	switch {
	case !contract.ValidSite(c.Site):
		return fmt.Errorf("powerapp: invalid site %q", c.Site)
	case c.DeviceID == "":
		return errors.New("powerapp: DeviceID is required (hub_device_id in the hub's configuration)")
	case c.PollEvery < 100*time.Millisecond || c.PollEvery > 10*time.Second:
		return errors.New("powerapp: PollEvery from 100 ms to 10 s")
	}
	if err := c.Outbox.Validate(); err != nil {
		return fmt.Errorf("powerapp: %w", err)
	}
	return c.Power.Validate()
}

// Deps are the ports the service needs.
type Deps struct {
	Clock     driver.Clock
	Store     driver.Store
	Publisher outbox.Publisher
	Board     Board
	NewID     func() string
	Logger    *slog.Logger
}

const (
	src      = "power"
	keyPower = "power"
	keySeq   = "power_seq"
)

// App is the application.
type App struct {
	cfg  Config
	deps Deps
	log  *slog.Logger

	core *power.Monitor
	loop *driver.Loop[power.Input, power.Action, power.Snapshot]
	out  *outbox.Outbox

	failing map[string]bool // the parts of the module that do not answer, for the poller only
	cuts    sync.WaitGroup

	life context.Context
	stop context.CancelFunc
	ran  atomic.Bool
}

// New builds the service.
func New(cfg Config, deps Deps) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.Clock == nil || deps.Store == nil || deps.Publisher == nil || deps.Board == nil || deps.NewID == nil {
		return nil, errors.New("powerapp: Clock, Store, Publisher, Board and NewID are required")
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	a := &App{cfg: cfg, deps: deps, log: log, failing: map[string]bool{}}
	a.life, a.stop = context.WithCancel(context.Background())
	var err error
	if a.core, err = power.New(cfg.Power); err != nil {
		return nil, err
	}
	if a.out, err = outbox.Open(deps.Store, deps.Publisher, keySeq, cfg.Site, cfg.Outbox, deps.NewID, log); err != nil {
		return nil, err
	}
	a.loop, err = driver.New(driver.Options[power.Input, power.Action, power.Snapshot]{
		Name: keyPower, Clock: deps.Clock, Store: deps.Store, Logger: log,
		Core: driver.Core[power.Input, power.Action, power.Snapshot]{
			Handle: a.core.Handle, NextWake: a.core.NextWake, Snapshot: a.core.Snapshot, Restore: a.core.Restore,
			Tick:    func(t time.Time) power.Input { return power.Tick{Now: t} },
			Stepped: a.core.Stepped,
		},
		Dispatch: a.dispatch,
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// Alive returns once the core's loop has taken what was queued for it (the
// watchdog).
func (a *App) Alive(ctx context.Context) error { return a.loop.Alive(ctx) }

// Run serves the service until ctx is cancelled. It must be called once.
func (a *App) Run(ctx context.Context) error {
	if !a.ran.CompareAndSwap(false, true) {
		return errors.New("powerapp: Run called twice")
	}
	defer a.stop()
	defer context.AfterFunc(ctx, a.stop)()
	var wg sync.WaitGroup
	for _, run := range []func(context.Context){func(c context.Context) { a.loop.Run(c) }, a.out.Run} {
		wg.Go(func() { run(a.life) })
	}
	a.poll(a.life) // the readings are told at once
	driver.Every(a.life, a.deps.Clock, a.cfg.PollEvery, a.poll)
	wg.Wait()
	a.cuts.Wait()
	a.out.Flush()
	return nil
}

// poll reads the module and hands the readings to the core. It runs on the poller's
// goroutine.
func (a *App) poll(ctx context.Context) {
	lost, err := a.deps.Board.MainsLost(ctx)
	if a.answered("mains line", err) {
		a.submit(func(now time.Time) power.Input { return power.Line{Now: now, Lost: lost} })
	}
	mv, percent, err := a.deps.Board.Gauge(ctx)
	if a.answered("fuel gauge", err) {
		a.submit(func(now time.Time) power.Input { return power.Gauge{Now: now, MilliVolts: mv, Percent: percent} })
	}
}

// answered logs when a part of the module stops answering, and when it answers again:
// not at every reading.
func (a *App) answered(part string, err error) bool {
	switch {
	case err != nil && !a.failing[part]:
		a.failing[part] = true
		a.log.Error("the battery module does not answer", "part", part, "err", err)
	case err == nil && a.failing[part]:
		delete(a.failing, part)
		a.log.Info("the battery module answers again", "part", part)
	}
	return err == nil
}

func (a *App) submit(build func(time.Time) power.Input) {
	if err := a.loop.Submit(a.life, build); err != nil && a.life.Err() == nil {
		a.log.Error("handing a reading to the power core failed", "err", err)
	}
}

// dispatch runs on the loop's goroutine.
func (a *App) dispatch(_ context.Context, now time.Time, actions []power.Action) error {
	for _, act := range actions {
		switch v := act.(type) {
		case power.Mains:
			a.log.Info("mains", "lost", v.Lost)
			a.publish(now, powerwire.Mains(a.cfg.DeviceID, v))
		case power.Battery:
			a.log.Info("battery", "percent", v.Percent)
			a.publish(now, powerwire.Battery(a.cfg.DeviceID, v))
		case power.Cut:
			a.cut(v)
		}
	}
	return nil
}

// publish sends a reading as an adapter sends a sensor's: the event, and the retained
// state the hub reads when it starts.
func (a *App) publish(now time.Time, d contract.DeviceEventData) {
	a.out.Send(src, contract.DeviceEventTopic(a.cfg.Site, d.DeviceID), contract.TypeDeviceEvent, d, false, now)
	a.out.Send(src, contract.DeviceStateTopic(a.cfg.Site, d.DeviceID, d.Capability), contract.TypeDeviceState, d, true, now)
}

// cut tells the module away from the loop: holding its button takes seconds.
func (a *App) cut(v power.Cut) {
	a.log.Error("the battery is nearly empty: asking the battery module to shut the hub down", "millivolts", v.MilliVolts)
	a.cuts.Go(func() {
		if err := a.deps.Board.Cut(a.life); err != nil && a.life.Err() == nil {
			a.log.Error("the battery module could not be told to shut the hub down", "err", err)
		}
	})
}
