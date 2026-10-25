// Package hub is the application layer of the hub: it assembles the domains (alarm
// engine, health monitor, access codes), feeds them with what arrives from the
// broker, and turns what they decide into messages.
//
// It is the only place that knows several domains at once. The domains know none
// of it: they receive plain inputs and return plain actions. Everything outside is
// a port (outbox.Publisher, driver.Clock, driver.Store) wired in the composition
// root (cmd/safehome-hub), so the hub is tested with fakes.
//
// Inputs go in through HandleMessage. Each domain runs in its own loop (see
// package driver) so nothing ever touches an engine from two goroutines. Outputs
// leave through an in-memory outbox that keeps the order and retries while the
// broker is away, so the alarm never waits for the network.
package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"safehome/internal/access"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/engine"
	"safehome/internal/escalation"
	"safehome/internal/health"
	"safehome/internal/inbox"
	"safehome/internal/outbox"
)

// Store keys.
const (
	keyEngine     = "engine"
	keyHealth     = "health"
	keyEscalation = "escalation"
	keyAccess     = "access"
	keySeq        = "seq"
)

// Hub is the application of the hub.
type Hub struct {
	cfg  Config
	deps Deps
	log  *slog.Logger

	// Each domain's loop owns its side: only that loop's goroutine touches it.
	alarm   alarmSide
	monitor healthSide
	escal   escalationSide
	startup bootState

	codes     *access.Registry
	installer *access.Registry // the installer code, apart from the people's
	reloading sync.Mutex       // one ReloadCodes at a time, so the last one read wins

	out  *outbox.Outbox
	seen *inbox.RecentIDs

	bootID  string
	started time.Time

	// Read from any goroutine.
	mode  atomic.Value                       // the alarm state, for the heartbeat
	mains atomic.Bool                        // a mains reading has been seen
	gsm   atomic.Pointer[contract.GSMHealth] // the GSM gateway's lines, once it spoke
	// faults is the monitor's latest summary, for the engine's loop: an arming that
	// starts names the silent devices and the weak batteries.
	faults atomic.Pointer[health.Status]

	auth chan order
	// checking counts the orders handed to the auth worker and not yet queued for the
	// engine: while there are some, the orders without a code queue behind them.
	checking atomic.Int32

	// life ends with Run: the loops run on it, and work handed to them after it ended
	// is given up quietly.
	life context.Context
	stop context.CancelFunc
	ran  atomic.Bool
}

// alarmSide is owned by the engine's loop.
type alarmSide struct {
	eng         *engine.Engine
	loop        *driver.Loop[engine.Input, engine.Action, engine.Snapshot]
	inst        installMode
	siren       sirenOrder
	sirenMemory // what is known of the sirens' failures (siren.go)
}

// healthSide is owned by the monitor's loop.
type healthSide struct {
	mon         *health.Monitor
	loop        *driver.Loop[health.Input, health.Action, health.Snapshot]
	uplink      health.LinkState // the last internet state given to the escalation
	lastHealth  []byte           // the last system.health published
	lastDevices []byte           // the last system.devices published
	booted      bool             // the boot is over: system.devices may go (see publishDevices)
}

// escalationSide is owned by the escalation's loop.
type escalationSide struct {
	esc  *escalation.Escalation
	loop *driver.Loop[escalation.Input, escalation.Action, escalation.Snapshot]
	// life is the escalation loop's own: it ends after the engine and the monitor (Run).
	life context.Context
	stop context.CancelFunc
}

// bootState is the boot window (see boot): the retained device states collected
// while it is open. HandleMessage and the boot share it, under mu.
type bootState struct {
	mu       sync.Mutex
	open     bool
	states   map[string]contract.DeviceEventData
	linkUp   chan struct{} // closed by LinkUp
	linkOnce sync.Once
}

// New builds the hub, loading what was saved (access codes, message counter,
// engine and monitor state). A damaged saved state is an error: the operator
// decides what to do, the hub does not silently start from nothing.
func New(cfg Config, deps Deps) (*Hub, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := deps.validate(); err != nil {
		return nil, err
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := deps.Clock.Now()
	h := &Hub{
		cfg: cfg, deps: deps, log: log,
		seen:    inbox.NewRecentIDs(cfg.DedupeSize),
		bootID:  deps.NewID(),
		started: now,
		startup: bootState{open: true, linkUp: make(chan struct{}), states: map[string]contract.DeviceEventData{}},
		auth:    make(chan order, cfg.AuthQueue),
	}
	h.life, h.stop = context.WithCancel(context.Background())
	h.escal.life, h.escal.stop = context.WithCancel(context.Background())
	h.mode.Store(string(engine.ModeDisarmed))

	if err := h.openSaved(); err != nil {
		return nil, err
	}
	if err := h.buildEngine(now); err != nil {
		return nil, err
	}
	if err := h.buildMonitor(now); err != nil {
		return nil, err
	}
	if err := h.buildEscalation(); err != nil {
		return nil, err
	}
	return h, nil
}

// openSaved opens what the hub keeps apart from the domains' states: the access
// codes and the message counter.
func (h *Hub) openSaved() (err error) {
	if h.codes, err = h.openPeopleCodes(); err != nil {
		return err
	}
	if h.installer, err = openInstallerCode(h.cfg.Access, h.deps.Store); err != nil {
		return err
	}
	h.out, err = outbox.Open(h.deps.Store, h.deps.Publisher, keySeq, h.cfg.Site, h.cfg.Outbox, h.deps.NewID, h.log)
	return err
}

// buildEngine builds the alarm engine and its loop, which puts its saved state back.
func (h *Hub) buildEngine(now time.Time) (err error) {
	if h.alarm.eng, err = engine.New(h.cfg.Engine, h.deps.NewID, now); err != nil {
		return err
	}
	h.alarm.loop, err = driver.New(driver.Options[engine.Input, engine.Action, engine.Snapshot]{
		Name: keyEngine, Clock: h.deps.Clock, Store: h.deps.Store, Logger: h.log,
		// The engine pulls back what a clock running ahead stamped: a clock corrected
		// backwards must not stretch an entry delay or a lockout by its step.
		FollowsClock: true,
		Core: driver.Core[engine.Input, engine.Action, engine.Snapshot]{
			Handle: h.alarm.eng.Handle, NextWake: h.alarm.eng.NextWake, Snapshot: h.alarm.eng.Snapshot, Restore: h.alarm.eng.Restore,
			Tick: func(t time.Time) engine.Input { return engine.Tick{Now: t} },
		},
		Dispatch: h.dispatchEngine,
		OnChange: func(_ context.Context, t time.Time) {
			h.publishState(t)
			h.installFollowsAlarm(t)
		},
	})
	return err
}

// buildMonitor builds the health monitor and its loop, which puts its saved state
// back.
func (h *Hub) buildMonitor(now time.Time) (err error) {
	if h.monitor.mon, err = health.New(h.cfg.healthConfig(), h.deps.NewID, now); err != nil {
		return err
	}
	h.monitor.loop, err = driver.New(driver.Options[health.Input, health.Action, health.Snapshot]{
		Name: keyHealth, Clock: h.deps.Clock, Store: h.deps.Store, Logger: h.log,
		Core: driver.Core[health.Input, health.Action, health.Snapshot]{
			Handle: h.monitor.mon.Handle, NextWake: h.monitor.mon.NextWake, Snapshot: h.monitor.mon.Snapshot, Restore: h.monitor.mon.Restore,
			Tick: func(t time.Time) health.Input { return health.Tick{Now: t} },
		},
		Dispatch: h.dispatchHealth,
		OnChange: func(_ context.Context, t time.Time) {
			h.publishHealth(t)
			h.publishDevices(t)
		},
	})
	if err != nil {
		return err
	}
	st := h.monitor.mon.Status() // restored by the loop; nothing runs yet
	h.faults.Store(&st)
	return nil
}

// buildEscalation builds the escalation and its loop, which puts its saved state
// back.
func (h *Hub) buildEscalation() (err error) {
	if h.escal.esc, err = escalation.New(h.cfg.Escalation); err != nil {
		return err
	}
	h.escal.loop, err = driver.New(driver.Options[escalation.Input, escalation.Action, escalation.Snapshot]{
		Name: keyEscalation, Clock: h.deps.Clock, Store: h.deps.Store, Logger: h.log,
		Core: driver.Core[escalation.Input, escalation.Action, escalation.Snapshot]{
			Handle: h.escal.esc.Handle, NextWake: h.escal.esc.NextWake, Snapshot: h.escal.esc.Snapshot, Restore: h.escal.esc.Restore,
			Tick: func(t time.Time) escalation.Input { return escalation.Tick{Now: t} },
		},
		Dispatch: h.dispatchEscalation,
	})
	return err
}

// LinkUp tells the hub that the subscriptions are active: the boot window, in which
// the retained device states arrive, starts now. Calling it again does nothing.
func (h *Hub) LinkUp() { h.startup.linkOnce.Do(func() { close(h.startup.linkUp) }) }

// Alive returns once the engine, the health monitor and the escalation have each
// taken what was queued for them: none of them is stuck. The program checks it before
// telling systemd that the hub still works (the watchdog).
func (h *Hub) Alive(ctx context.Context) error {
	for _, l := range []struct {
		name  string
		alive func(context.Context) error
	}{{"engine", h.alarm.loop.Alive}, {"health", h.monitor.loop.Alive}, {"escalation", h.escal.loop.Alive}} {
		if err := l.alive(ctx); err != nil {
			return fmt.Errorf("hub: %s does not answer: %w", l.name, err)
		}
	}
	return nil
}

// Subscriptions lists what the hub must listen to on the broker.
func (h *Hub) Subscriptions() []inbox.Subscription {
	site := h.cfg.Site
	subs := []inbox.Subscription{
		{Filter: contract.Topic(site, "device", "+", "event"), QoS: 1},
		{Filter: contract.Topic(site, "device", "+", "state", "+"), QoS: 1},
		{Filter: contract.AlarmCmdTopic(site), QoS: 1},
		{Filter: contract.AlertResultTopic(site), QoS: 1},
		{Filter: contract.AlertAckTopic(site), QoS: 1},
		{Filter: contract.AlertSeenTopic(site), QoS: 1},
		{Filter: contract.GSMStatusTopic(site), QoS: 1},
		{Filter: contract.ZigbeeStatusTopic(site), QoS: 1},
		{Filter: contract.AgentStatusTopic(site), QoS: 1},
		{Filter: contract.InstallCmdTopic(site), QoS: 1},
		{Filter: contract.RegistryEventTopic(site), QoS: 1},
		{Filter: contract.RegistryDevicesTopic(site), QoS: 1},
	}
	for _, id := range h.cfg.SirenIDs {
		subs = append(subs, inbox.Subscription{Filter: contract.DeviceAckTopic(site, id), QoS: 1})
	}
	return subs
}

// Run serves the hub until ctx is cancelled. It must be called once. Messages may
// be given to HandleMessage as soon as Run has started.
func (h *Hub) Run(ctx context.Context) error {
	if !h.ran.CompareAndSwap(false, true) {
		return errors.New("hub: Run called twice")
	}
	// Everything runs on h.life, which ends with ctx: a loop only stops once h.life
	// is over, so a message handed to it meanwhile is known to be given up.
	defer h.stop()
	defer h.escal.stop()
	defer context.AfterFunc(ctx, h.stop)()

	var domains, rest sync.WaitGroup
	domains.Go(func() { h.alarm.loop.Run(h.life) })
	domains.Go(func() { h.monitor.loop.Run(h.life) })
	rest.Go(func() { h.escal.loop.Run(h.escal.life) })
	for _, run := range []func(context.Context){
		h.out.Run,
		h.authWorker,
		func(c context.Context) { driver.Every(c, h.deps.Clock, h.cfg.HeartbeatEvery, h.publishHeartbeat) },
	} {
		rest.Go(func() { run(h.life) })
	}

	if err := h.boot(h.life); err == nil {
		<-h.life.Done()
	}
	h.stop()
	// The escalation stops last: an alarm the engine raises or ends while it stops (a
	// task it took, a deadline due then) is saved as done, and must reach it too.
	domains.Wait()
	h.escal.stop()
	rest.Wait()
	h.out.Flush()
	return nil
}

// boot lets the retained device states arrive, then tells the engine what the
// devices look like, so a door left open is known before the first event.
func (h *Hub) boot(ctx context.Context) error {
	if err := h.bootWindow(ctx); err != nil {
		return err
	}
	signals := h.endBootWindow()
	err := h.alarm.loop.Do(ctx, func(now time.Time) []engine.Action {
		actions := h.alarm.eng.Boot(now, signals)
		h.publishState(now) // the first retained state, even if nothing changed since the last run
		// An installation mode never survives a restart: one left open by a crash is
		// closed for the console too. One opened at the keypad during the boot window
		// is this run's own: it stands, and the console keeps its network.
		if !h.alarm.inst.open {
			h.publishInstall(now, contract.InstallRestart)
		}
		return actions
	})
	if err != nil {
		return err
	}
	return h.monitor.loop.Do(ctx, func(now time.Time) []health.Action {
		h.publishHealth(now)
		h.monitor.booted = true
		h.publishDevices(now)
		h.publishContacts(now)
		return nil
	})
}

// bootWindow waits for the link to come up (LinkWait at most), then for the retained
// device states to arrive (BootWindow).
func (h *Hub) bootWindow(ctx context.Context) error {
	if h.cfg.BootWindow == 0 {
		return nil
	}
	if h.cfg.LinkWait > 0 {
		gaveUp, stop := h.deps.Clock.Timer(h.cfg.LinkWait)
		defer stop()
		select {
		case <-h.startup.linkUp:
		case <-gaveUp:
			h.log.Warn("the broker link did not come up in time, listening for device states anyway")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	over, stop := h.deps.Clock.Timer(h.cfg.BootWindow)
	defer stop()
	select {
	case <-over:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// endBootWindow stops collecting retained states and returns what they say to the
// engine, in a stable order, each dated when it began (see retainedSignal).
func (h *Hub) endBootWindow() []engine.Signal {
	h.startup.mu.Lock()
	h.startup.open = false
	states := h.startup.states
	h.startup.states = nil
	h.startup.mu.Unlock()

	var signals []engine.Signal
	for _, key := range slices.Sorted(maps.Keys(states)) {
		if sig, ok := retainedSignal(states[key]); ok {
			signals = append(signals, sig)
		}
	}
	return signals
}
