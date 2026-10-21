// Package hub is the application layer of the hub: it assembles the domains (alarm
// engine, health monitor, access codes), feeds them with what arrives from the
// broker, and turns what they decide into messages.
//
// It is the only place that knows several domains at once. The domains know none
// of it: they receive plain inputs and return plain actions. Everything outside is
// a port declared here (Publisher, driver.Clock, driver.Store) and wired in the
// composition root (cmd/safehome-hub), so the hub is tested with fakes.
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

// deliveriesKept is how many alert requests the hub remembers, to match their results.
const deliveriesKept = 2048

// Hub is the application of the hub.
type Hub struct {
	cfg  Config
	deps Deps
	log  *slog.Logger

	codes     *access.Registry
	installer *access.Registry // the installer code, apart from the people's
	inst      installMode      // owned by the engine's loop

	eng     *engine.Engine
	engLoop *driver.Loop[engine.Input, engine.Action, engine.Snapshot]
	mon     *health.Monitor
	monLoop *driver.Loop[health.Input, health.Action, health.Snapshot]
	esc     *escalation.Escalation
	escLoop *driver.Loop[escalation.Input, escalation.Action, escalation.Snapshot]
	// deliveries tells which escalation message an alert request carries.
	deliveries *deliveryIndex

	out  *outbox.Outbox
	seen *outbox.RecentIDs

	bootID  string
	started time.Time
	mode    atomic.Value // the alarm state, for the heartbeat
	mains   atomic.Bool  // a mains reading has been seen

	gsm atomic.Pointer[contract.GSMHealth] // the GSM gateway's lines, once it spoke

	// uplink is the last internet state given to the escalation; owned by the
	// monitor's loop.
	uplink health.LinkState

	// faults is the monitor's latest summary, for the engine's loop: an arming that
	// starts names the silent devices and the weak batteries.
	faults atomic.Pointer[health.Status]

	bootMu     sync.Mutex
	booting    bool
	linkUp     chan struct{}
	linkOnce   sync.Once
	bootStates map[string]contract.DeviceEventData

	auth chan authJob

	// life ends with Run: the loops run on it, and work handed to them after it ended
	// is given up quietly.
	life context.Context
	stop context.CancelFunc
	ran  atomic.Bool
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
		seen:       outbox.NewRecentIDs(cfg.DedupeSize),
		deliveries: newDeliveryIndex(deliveriesKept),
		bootID:     deps.NewID(),
		started:    now,
		booting:    true,
		linkUp:     make(chan struct{}),
		bootStates: map[string]contract.DeviceEventData{},
		auth:       make(chan authJob, cfg.AuthQueue),
	}
	h.life, h.stop = context.WithCancel(context.Background())
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
	if h.installer, err = OpenInstallerCode(h.cfg.Access, h.deps.Store); err != nil {
		return err
	}
	seq, err := outbox.NewSequencer(h.deps.Store, keySeq, h.log)
	if err != nil {
		return err
	}
	h.out = outbox.New(h.deps.Publisher, seq, h.cfg.Site, h.cfg.OutboxSize, h.cfg.RetryMin, h.cfg.RetryMax, h.log)
	return nil
}

// buildEngine builds the alarm engine and its loop, which puts its saved state back.
func (h *Hub) buildEngine(now time.Time) (err error) {
	if h.eng, err = engine.New(h.cfg.Engine, h.deps.NewID, now); err != nil {
		return err
	}
	h.engLoop, err = driver.New(driver.Options[engine.Input, engine.Action, engine.Snapshot]{
		Name: keyEngine, Clock: h.deps.Clock, Store: h.deps.Store, Logger: h.log,
		Core: driver.Core[engine.Input, engine.Action, engine.Snapshot]{
			Handle: h.eng.Handle, NextWake: h.eng.NextWake, Snapshot: h.eng.Snapshot, Restore: h.eng.Restore,
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
	if h.mon, err = health.New(h.cfg.healthConfig(), h.deps.NewID, now); err != nil {
		return err
	}
	h.monLoop, err = driver.New(driver.Options[health.Input, health.Action, health.Snapshot]{
		Name: keyHealth, Clock: h.deps.Clock, Store: h.deps.Store, Logger: h.log,
		Core: driver.Core[health.Input, health.Action, health.Snapshot]{
			Handle: h.mon.Handle, NextWake: h.mon.NextWake, Snapshot: h.mon.Snapshot, Restore: h.mon.Restore,
			Tick: func(t time.Time) health.Input { return health.Tick{Now: t} },
		},
		Dispatch: h.dispatchHealth,
		OnChange: func(_ context.Context, t time.Time) { h.publishHealth(t) },
	})
	if err != nil {
		return err
	}
	st := h.mon.Status() // restored by the loop; nothing runs yet
	h.faults.Store(&st)
	return nil
}

// buildEscalation builds the escalation and its loop, which puts its saved state
// back.
func (h *Hub) buildEscalation() (err error) {
	if h.esc, err = escalation.New(h.cfg.Escalation); err != nil {
		return err
	}
	h.escLoop, err = driver.New(driver.Options[escalation.Input, escalation.Action, escalation.Snapshot]{
		Name: keyEscalation, Clock: h.deps.Clock, Store: h.deps.Store, Logger: h.log,
		Core: driver.Core[escalation.Input, escalation.Action, escalation.Snapshot]{
			Handle: h.esc.Handle, NextWake: h.esc.NextWake, Snapshot: h.esc.Snapshot, Restore: h.esc.Restore,
			Tick: func(t time.Time) escalation.Input { return escalation.Tick{Now: t} },
		},
		Dispatch: h.dispatchEscalation,
	})
	return err
}

// LinkUp tells the hub that the subscriptions are active: the boot window, in which
// the retained device states arrive, starts now. Calling it again does nothing.
func (h *Hub) LinkUp() { h.linkOnce.Do(func() { close(h.linkUp) }) }

// Alive returns once the engine, the health monitor and the escalation have each
// taken what was queued for them: none of them is stuck. The program checks it before
// telling systemd that the hub still works (the watchdog).
func (h *Hub) Alive(ctx context.Context) error {
	for _, l := range []struct {
		name  string
		alive func(context.Context) error
	}{{"engine", h.engLoop.Alive}, {"health", h.monLoop.Alive}, {"escalation", h.escLoop.Alive}} {
		if err := l.alive(ctx); err != nil {
			return fmt.Errorf("hub: %s does not answer: %w", l.name, err)
		}
	}
	return nil
}

// Subscriptions lists what the hub must listen to on the broker.
func (h *Hub) Subscriptions() []Subscription {
	site := h.cfg.Site
	return []Subscription{
		{Filter: contract.Topic(site, "device", "+", "event"), QoS: 1},
		{Filter: contract.Topic(site, "device", "+", "state", "+"), QoS: 1},
		{Filter: contract.AlarmCmdTopic(site), QoS: 1},
		{Filter: contract.AlertResultTopic(site), QoS: 1},
		{Filter: contract.AlertAckTopic(site), QoS: 1},
		{Filter: contract.GSMStatusTopic(site), QoS: 1},
		{Filter: contract.ZigbeeStatusTopic(site), QoS: 1},
		{Filter: contract.AgentStatusTopic(site), QoS: 1},
		{Filter: contract.InstallCmdTopic(site), QoS: 1},
	}
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
	defer context.AfterFunc(ctx, h.stop)()

	var wg sync.WaitGroup
	for _, run := range []func(context.Context){
		func(c context.Context) { h.engLoop.Run(c) },
		func(c context.Context) { h.monLoop.Run(c) },
		func(c context.Context) { h.escLoop.Run(c) },
		h.out.Run,
		h.authWorker,
		h.heartbeats,
	} {
		wg.Go(func() { run(h.life) })
	}

	if err := h.boot(h.life); err == nil {
		<-h.life.Done()
	}
	h.stop()
	wg.Wait()
	h.out.Flush(3 * time.Second)
	return nil
}

// boot lets the retained device states arrive, then tells the engine what the
// devices look like, so a door left open is known before the first event.
func (h *Hub) boot(ctx context.Context) error {
	if err := h.bootWindow(ctx); err != nil {
		return err
	}
	signals := h.endBootWindow()
	err := h.engLoop.Do(ctx, func(now time.Time) []engine.Action {
		actions := h.eng.Boot(now, signals)
		h.publishState(now) // the first retained state, even if nothing changed since the last run
		// An installation mode never survives a restart: one left open by a crash is
		// closed for the console too.
		h.publishInstall(now, contract.InstallRestart)
		return actions
	})
	if err != nil {
		return err
	}
	return h.monLoop.Do(ctx, func(now time.Time) []health.Action {
		h.publishHealth(now)
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
		select {
		case <-h.linkUp:
		case <-time.After(h.cfg.LinkWait):
			h.log.Warn("the broker link did not come up in time, listening for device states anyway")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !outbox.Sleep(ctx, h.cfg.BootWindow) {
		return ctx.Err()
	}
	return nil
}

// endBootWindow stops collecting retained states and returns what they say to the
// engine, in a stable order.
func (h *Hub) endBootWindow() []engine.Signal {
	h.bootMu.Lock()
	h.booting = false
	states := h.bootStates
	h.bootStates = nil
	h.bootMu.Unlock()

	var signals []engine.Signal
	for _, key := range slices.Sorted(maps.Keys(states)) {
		if sig, ok := signalOf(states[key]); ok {
			signals = append(signals, sig)
		}
	}
	return signals
}
