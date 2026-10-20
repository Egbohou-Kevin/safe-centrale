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
	"encoding/json"
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

// Config holds everything the hub needs to know about its site.
type Config struct {
	Site        string // for example "tg-lom-0001"
	HubDeviceID string // the hub's battery module: its mains and battery readings are the hub's
	SirenIDs    []string

	Engine engine.Config
	Health health.Config // HubDeviceID is taken from the field above
	Access access.Config
	// Escalation holds the contacts and the plans: who is told, and how.
	Escalation escalation.Config

	HeartbeatEvery time.Duration // sign of life, one minute by default
	BootWindow     time.Duration // how long to listen for retained device states at start
	// LinkWait is how long the boot waits for LinkUp before listening anyway: the
	// window only makes sense once the subscriptions are active. Zero does not wait.
	LinkWait        time.Duration
	SirenCommandTTL time.Duration // a siren command not carried out within this is void

	DedupeSize int           // message ids remembered
	OutboxSize int           // messages kept while the broker is away
	RetryMin   time.Duration // first wait before sending again
	RetryMax   time.Duration // longest wait
	AuthQueue  int           // code checks that may wait

	Versions map[string]string // component -> version, for the heartbeat
}

// DefaultConfig returns the defaults decided in the design documents.
func DefaultConfig(site, hubDeviceID string, sirenIDs ...string) Config {
	return Config{
		Site: site, HubDeviceID: hubDeviceID, SirenIDs: sirenIDs,
		Engine: engine.DefaultConfig(), Health: health.DefaultConfig(), Access: access.DefaultConfig(),
		Escalation:     escalation.DefaultConfig(site, nil),
		HeartbeatEvery: time.Minute, BootWindow: 2 * time.Second, LinkWait: 30 * time.Second, SirenCommandTTL: 30 * time.Second,
		DedupeSize: 2048, OutboxSize: 2000, RetryMin: 200 * time.Millisecond, RetryMax: 5 * time.Second,
		AuthQueue: 16,
	}
}

// Validate checks the whole configuration, domains included, without building
// anything.
func (c Config) Validate() error {
	if err := c.validate(); err != nil {
		return err
	}
	if err := c.Engine.Validate(); err != nil {
		return err
	}
	hc := c.Health
	hc.HubDeviceID = c.HubDeviceID
	if err := hc.Validate(); err != nil {
		return err
	}
	return c.Access.Validate()
}

func (c Config) validate() error {
	switch {
	case !contract.ValidSite(c.Site):
		return fmt.Errorf("hub: invalid site %q", c.Site)
	case c.HubDeviceID == "":
		return errors.New("hub: HubDeviceID is required")
	case len(c.SirenIDs) == 0:
		return errors.New("hub: at least one siren is required: an alarm that cannot sound is a misconfiguration")
	case c.HeartbeatEvery <= 0 || c.SirenCommandTTL <= 0 || c.RetryMin <= 0 || c.RetryMax < c.RetryMin:
		return errors.New("hub: durations must be positive")
	case c.BootWindow < 0:
		return errors.New("hub: BootWindow must not be negative")
	case c.Escalation.Validate() != nil:
		return fmt.Errorf("hub: %w", c.Escalation.Validate())
	case c.LinkWait < 0:
		return errors.New("hub: LinkWait must not be negative")
	case c.DedupeSize < 1 || c.OutboxSize < 1 || c.AuthQueue < 1:
		return errors.New("hub: DedupeSize, OutboxSize and AuthQueue must be at least 1")
	}
	for _, id := range c.SirenIDs {
		if id == "" {
			return errors.New("hub: empty siren id")
		}
	}
	return nil
}

// Publisher is the port the hub needs from the MQTT adapter (see package outbox).
type Publisher = outbox.Publisher

// ErrUnavailable is what a Publisher returns when the broker cannot be reached right
// now: the message is kept and sent again.
var ErrUnavailable = outbox.ErrUnavailable

// Deps are the ports the hub needs.
type Deps struct {
	Clock     driver.Clock
	Store     driver.Store
	Publisher Publisher
	NewID     func() string // message and incident identifiers (ULIDs)
	Logger    *slog.Logger
	// ClockSynced says whether the system clock is synchronised, for the heartbeat;
	// known is false when it cannot tell. Nil leaves clock_ok out.
	ClockSynced func() (synced, known bool)
	// DevicesChanged says whether the installer's tools changed the zones or the
	// sirens since the hub loaded them; RestartWhenQuiet has the hub restart to load
	// them, at a quiet moment. Asked when the installation mode closes; both or
	// neither.
	DevicesChanged   func() bool
	RestartWhenQuiet func()
}

// Message is a message received from the broker.
type Message struct {
	Topic   string
	Payload []byte
	Retain  bool
}

// Subscription is a topic filter the hub wants to listen to.
type Subscription struct {
	Filter string
	QoS    byte
}

// Store keys.
const (
	keyEngine     = "engine"
	keyHealth     = "health"
	keyEscalation = "escalation"
	keyAccess     = "access"
	keySeq        = "seq"
)

// Sources written in the envelope.
const (
	srcEngine = "engine"
	srcHealth = "health"
)

const maxPayload = 64 * 1024

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

	gsmMu sync.Mutex
	gsm   *contract.GSMHealth // the GSM gateway's lines, once it spoke

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

	life context.Context // cancelled when Run ends: queued work is given up
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
	if deps.Clock == nil || deps.Store == nil || deps.Publisher == nil || deps.NewID == nil {
		return nil, errors.New("hub: Clock, Store, Publisher and NewID are required")
	}
	if (deps.DevicesChanged == nil) != (deps.RestartWhenQuiet == nil) {
		return nil, errors.New("hub: DevicesChanged and RestartWhenQuiet go together")
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	h := &Hub{
		cfg: cfg, deps: deps, log: log,
		seen:       outbox.NewRecentIDs(cfg.DedupeSize),
		booting:    true,
		linkUp:     make(chan struct{}),
		bootStates: map[string]contract.DeviceEventData{},
		auth:       make(chan authJob, cfg.AuthQueue),
		deliveries: newDeliveryIndex(2048),
	}
	h.life, h.stop = context.WithCancel(context.Background())
	h.bootID = deps.NewID()
	now := deps.Clock.Now()
	h.started = now
	h.mode.Store(string(engine.ModeDisarmed))

	codes, err := h.openCodes()
	if err != nil {
		return nil, err
	}
	h.codes = codes
	if h.installer, err = OpenInstallerCode(cfg.Access, deps.Store); err != nil {
		return nil, err
	}

	seq, err := outbox.NewSequencer(deps.Store, keySeq, log)
	if err != nil {
		return nil, err
	}
	h.out = outbox.New(deps.Publisher, seq, cfg.Site, cfg.OutboxSize, cfg.RetryMin, cfg.RetryMax, log)

	if h.eng, err = engine.New(cfg.Engine, deps.NewID, now); err != nil {
		return nil, err
	}
	hc := cfg.Health
	hc.HubDeviceID = cfg.HubDeviceID
	if h.mon, err = health.New(hc, deps.NewID, now); err != nil {
		return nil, err
	}

	if h.esc, err = escalation.New(cfg.Escalation); err != nil {
		return nil, err
	}

	h.engLoop, err = driver.New(driver.Options[engine.Input, engine.Action, engine.Snapshot]{
		Name: keyEngine, Clock: deps.Clock, Store: deps.Store, Logger: log,
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
	if err != nil {
		return nil, err
	}
	h.monLoop, err = driver.New(driver.Options[health.Input, health.Action, health.Snapshot]{
		Name: keyHealth, Clock: deps.Clock, Store: deps.Store, Logger: log,
		Core: driver.Core[health.Input, health.Action, health.Snapshot]{
			Handle: h.mon.Handle, NextWake: h.mon.NextWake, Snapshot: h.mon.Snapshot, Restore: h.mon.Restore,
			Tick: func(t time.Time) health.Input { return health.Tick{Now: t} },
		},
		Dispatch: h.dispatchHealth,
		OnChange: func(_ context.Context, t time.Time) { h.publishHealth(t) },
	})
	if err != nil {
		return nil, err
	}
	st := h.mon.Status() // restored by the loop; nothing runs yet
	h.faults.Store(&st)
	h.escLoop, err = driver.New(driver.Options[escalation.Input, escalation.Action, escalation.Snapshot]{
		Name: keyEscalation, Clock: deps.Clock, Store: deps.Store, Logger: log,
		Core: driver.Core[escalation.Input, escalation.Action, escalation.Snapshot]{
			Handle: h.esc.Handle, NextWake: h.esc.NextWake, Snapshot: h.esc.Snapshot, Restore: h.esc.Restore,
			Tick: func(t time.Time) escalation.Input { return escalation.Tick{Now: t} },
		},
		Dispatch: h.dispatchEscalation,
	})
	if err != nil {
		return nil, err
	}
	return h, nil
}

// Codes returns the access code registry, for the installer's tools.
func (h *Hub) Codes() *access.Registry { return h.codes }

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
	return []Subscription{
		{Filter: contract.Topic(h.cfg.Site, "device", "+", "event"), QoS: 1},
		{Filter: contract.Topic(h.cfg.Site, "device", "+", "state", "+"), QoS: 1},
		{Filter: contract.AlarmCmdTopic(h.cfg.Site), QoS: 1},
		{Filter: contract.AlertResultTopic(h.cfg.Site), QoS: 1},
		{Filter: contract.AlertAckTopic(h.cfg.Site), QoS: 1},
		{Filter: contract.GSMStatusTopic(h.cfg.Site), QoS: 1},
		{Filter: contract.ZigbeeStatusTopic(h.cfg.Site), QoS: 1},
		{Filter: contract.AgentStatusTopic(h.cfg.Site), QoS: 1},
		{Filter: contract.InstallCmdTopic(h.cfg.Site), QoS: 1},
	}
}

// The link names and states the hub understands.
const (
	linkInternet = "internet"
	linkGSM      = "gsm"
	linkZigbee   = "zigbee"
	linkOK       = "ok"
	linkDown     = "down"
	linkSilent   = "silent" // the component stopped giving news (it is dead or stuck)
)

// Run serves the hub until ctx is cancelled. It must be called once. Messages may
// be given to HandleMessage as soon as Run has started.
func (h *Hub) Run(ctx context.Context) error {
	if !h.ran.CompareAndSwap(false, true) {
		return errors.New("hub: Run called twice")
	}
	defer h.stop()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	start := func(f func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); f(runCtx) }()
	}
	start(func(c context.Context) { h.engLoop.Run(c) })
	start(func(c context.Context) { h.monLoop.Run(c) })
	start(func(c context.Context) { h.escLoop.Run(c) })
	start(h.out.Run)
	start(h.authWorker)
	start(h.heartbeats)

	if err := h.boot(runCtx); err == nil {
		<-runCtx.Done()
	}
	cancel()
	wg.Wait()
	h.out.Flush(3 * time.Second)
	return nil
}

// boot lets the retained device states arrive, then tells the engine what the
// devices look like, so a door left open is known before the first event.
func (h *Hub) boot(ctx context.Context) error {
	if h.cfg.BootWindow > 0 {
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
	}
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

// --- access codes -----------------------------------------------------------

func (h *Hub) openCodes() (*access.Registry, error) { return OpenCodes(h.cfg.Access, h.deps.Store) }

// OpenCodes opens the access codes saved in store. The hub uses it at start, and so
// does the installer's command-line tool, which sets codes without running a hub.
// Every change is saved before it takes effect.
func OpenCodes(cfg access.Config, store driver.Store) (*access.Registry, error) {
	return openRegistry(cfg, store, keyAccess)
}

func openRegistry(cfg access.Config, store driver.Store, key string) (*access.Registry, error) {
	records, err := loadCodes(store, key)
	if err != nil {
		return nil, err
	}
	persist := func(rs []access.Record) error {
		b, err := json.Marshal(rs)
		if err != nil {
			return err
		}
		return store.Save(key, b)
	}
	return access.New(cfg, records, persist)
}

func loadCodes(store driver.Store, key string) ([]access.Record, error) {
	data, found, err := store.Load(key)
	if err != nil {
		return nil, fmt.Errorf("hub: loading the access codes: %w", err)
	}
	if !found {
		return nil, nil
	}
	var records []access.Record
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("hub: reading the access codes: %w", err)
	}
	return records, nil
}

// ReloadCodes reads the saved codes again. The installer's tool writes them from
// another process, and the composition root calls this when told to (SIGHUP). A
// damaged file leaves the codes in use untouched and is reported.
func (h *Hub) ReloadCodes() error {
	records, err := loadCodes(h.deps.Store, keyAccess)
	if err != nil {
		return err
	}
	installer, err := loadCodes(h.deps.Store, keyInstaller)
	if err != nil {
		return err
	}
	if err := h.codes.Reload(records); err != nil {
		return err
	}
	if err := h.installer.Reload(installer); err != nil {
		return err
	}
	h.log.Info("access codes reloaded", "users", len(records), "installer", len(installer) > 0)
	return nil
}

// --- heartbeat ----------------------------------------------------------------

func (h *Hub) heartbeats(ctx context.Context) {
	for {
		fired, stop := h.deps.Clock.Timer(h.cfg.HeartbeatEvery)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-fired:
			h.publishHeartbeat()
		}
	}
}

func (h *Hub) publishHeartbeat() {
	now := h.deps.Clock.Now()
	mode, _ := h.mode.Load().(string)
	data := contract.HeartbeatData{
		BootID:     h.bootID,
		UptimeS:    max(int64(now.Sub(h.started)/time.Second), 0),
		Engine:     mode,
		QueueDepth: h.out.Depth(),
		FW:         h.cfg.Versions,
	}
	if h.deps.ClockSynced != nil {
		if synced, known := h.deps.ClockSynced(); known {
			data.ClockOK = &synced
		}
	}
	h.enqueue(srcEngine, contract.HeartbeatTopic(h.cfg.Site), contract.TypeHeartbeat, data, 1, false, now)
}

// enqueue puts a message in the outbox.
// It returns the identifier of the message, empty if it could not be built.
func (h *Hub) enqueue(src, topic, typ string, data any, qos byte, retain bool, ts time.Time) string {
	raw, err := json.Marshal(data)
	if err != nil {
		h.log.Error("encoding a message failed", "type", typ, "err", err)
		return ""
	}
	id := h.deps.NewID()
	h.out.Put(&outbox.Item{Topic: topic, Src: src, Type: typ, Data: raw, QoS: qos, Retain: retain, ID: id, TS: ts})
	return id
}
