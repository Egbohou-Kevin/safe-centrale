// Package configapp applies the site's configuration set from the cloud
// (config.set, see docs/contract.md). It runs in the hub's program: it checks a new
// configuration against the site's files, keeps it in the state folder, and has the
// hub restart to load it, at a quiet moment: disarmed or armed, never during an entry
// delay, an alarm, nor the installation mode; when the zones or the sirens changed,
// during the exit delay rather than armed (see QuietState). The engine itself says
// whether the moment is quiet: the alarm/state it publishes comes back through the
// broker, behind it. It
// answers with config.applied, retained, once the hub runs with it, or at once when it
// refuses it. The zones and sirens changed by the installer's tools are applied the
// same way.
//
// The work (a configuration checked and written to disk, the engine asked) is done on
// Run's goroutine, never on the broker's, which would hold up every other message of
// the hub meanwhile.
//
// The other programs that read this configuration (the GSM gateway, the video
// service) restart when config.applied names a version they do not run.
//
// Checking, keeping and restarting are ports, wired by the composition root
// (cmd/safehome-hub): this package knows no file and no process.
package configapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/inbox"
	"safehome/internal/outbox"
)

// Config holds what the application needs to know.
type Config struct {
	Site string
	// Running is the version the hub runs with (0: its files alone). Refused, when
	// set, is the version found in the state folder that the hub could not load, and
	// why: it is refused again at start.
	Running        int
	Refused        int
	RefusedBecause string

	Outbox     outbox.Settings
	DedupeSize int
}

// DefaultConfig returns the defaults.
func DefaultConfig(site string, running int) Config {
	return Config{Site: site, Running: running, Outbox: outbox.DefaultSettings(50), DedupeSize: 64}
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if !contract.ValidSite(c.Site) {
		return fmt.Errorf("configapp: invalid site %q", c.Site)
	}
	if c.DedupeSize < 1 {
		return errors.New("configapp: DedupeSize must be set")
	}
	if err := c.Outbox.Validate(); err != nil {
		return fmt.Errorf("configapp: %w", err)
	}
	return nil
}

// Deps are the ports the application needs.
type Deps struct {
	Clock     driver.Clock
	Store     driver.Store // the message counter
	Publisher outbox.Publisher
	NewID     func() string
	// Check says whether the programs can run with this configuration.
	Check func(contract.ConfigSetData) error
	// Save keeps it for the next start of the programs.
	Save func(contract.ConfigSetData) error
	// Restart has the hub's program stop, to be started again with it.
	Restart func()
	// Quiet says, from the engine itself, whether the moment is quiet now: the alarm's
	// state (contract.State*) one that quiet accepts (see QuietState), no installation
	// mode. It waits for the inputs queued before it (a door opened a moment ago), until
	// ctx ends: false then.
	Quiet func(ctx context.Context, quiet func(state string) bool) bool
	// DevicesChanged says whether the installer's tools (the local console, the
	// command-line tool) changed the zones or the sirens since the hub loaded them: the
	// hub restarts for them too. Asked at each quiet moment; nil when none can change.
	DevicesChanged func() bool
	// Logger: never given a configuration, which holds phone numbers.
	Logger *slog.Logger
}

const (
	src        = "config"
	keySeq     = "config_seq"
	maxPayload = 64 * 1024
	// queueSize bounds the messages waiting for Run's goroutine: configurations are
	// rare, and the alarm's states come one at a time.
	queueSize = 256
	// quietWait bounds the wait for the engine's answer: a busy engine is no quiet
	// moment, and its next state asks again.
	quietWait = 5 * time.Second
)

// App is the application.
type App struct {
	cfg  Config
	deps Deps
	log  *slog.Logger
	out  *outbox.Outbox
	seen *inbox.RecentIDs
	work chan func(context.Context) // run in order on Run's goroutine

	mu         sync.Mutex
	saved      int    // the version kept, waiting for a quiet moment (0: none)
	state      string // the alarm's state, from alarm/state ("" until known)
	installing bool   // the installation mode is open, from install/mode
	restarted  bool
}

// New builds the application.
func New(cfg Config, deps Deps) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.Clock == nil || deps.Store == nil || deps.Publisher == nil || deps.NewID == nil ||
		deps.Check == nil || deps.Save == nil || deps.Restart == nil || deps.Quiet == nil {
		return nil, errors.New("configapp: Clock, Store, Publisher, NewID, Check, Save, Restart and Quiet are required")
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	out, err := outbox.Open(deps.Store, deps.Publisher, keySeq, cfg.Site, cfg.Outbox, deps.NewID, log)
	if err != nil {
		return nil, err
	}
	return &App{cfg: cfg, deps: deps, log: log, out: out, seen: inbox.NewRecentIDs(cfg.DedupeSize),
		work: make(chan func(context.Context), queueSize)}, nil
}

// Subscriptions lists what the application listens to: the configuration, and the
// alarm's state and the installation mode, to restart at a quiet moment.
func (a *App) Subscriptions() []inbox.Subscription {
	return []inbox.Subscription{
		{Filter: contract.ConfigSetTopic(a.cfg.Site), QoS: 1},
		{Filter: contract.AlarmStateTopic(a.cfg.Site), QoS: 1},
		{Filter: contract.InstallModeTopic(a.cfg.Site), QoS: 1},
	}
}

// Run serves the application until ctx is cancelled. At start it says which
// configuration the hub runs with, or which one it could not load.
func (a *App) Run(ctx context.Context) error {
	now := a.deps.Clock.Now()
	switch {
	case a.cfg.Refused > 0:
		a.log.Error("the configuration from the cloud could not be loaded: the hub runs on its own files",
			"version", a.cfg.Refused, "err", a.cfg.RefusedBecause)
		a.answer(now, a.cfg.Refused, a.cfg.RefusedBecause)
	case a.cfg.Running > 0:
		a.log.Info("running with the configuration from the cloud", "version", a.cfg.Running)
		a.answer(now, a.cfg.Running, "")
	}
	sending := make(chan struct{})
	go func() { defer close(sending); a.out.Run(ctx) }()
	a.serve(ctx)
	<-sending
	a.out.Flush()
	return nil
}

// serve does the work handed by HandleMessage until ctx ends, then what is left: a
// configuration taken from the broker (and acknowledged) before the stop is still
// kept. Nothing restarts then.
func (a *App) serve(ctx context.Context) {
	for {
		select {
		case do := <-a.work:
			do(ctx)
		case <-ctx.Done():
			for {
				select {
				case do := <-a.work:
					do(ctx)
				default:
					return
				}
			}
		}
	}
}

// HandleMessage takes a message from the broker. It returns at once: the work is
// done on Run's goroutine, in the order received.
func (a *App) HandleMessage(m inbox.Message) {
	if len(m.Payload) > maxPayload {
		return
	}
	var do func(context.Context, inbox.Message)
	switch m.Topic {
	case contract.AlarmStateTopic(a.cfg.Site):
		do = a.alarmState
	case contract.InstallModeTopic(a.cfg.Site):
		do = a.installMode
	case contract.ConfigSetTopic(a.cfg.Site):
		do = a.configSet
	default:
		return
	}
	select {
	case a.work <- func(ctx context.Context) { do(ctx, m) }:
	default:
		a.log.Warn("too many messages waiting: one is dropped", "topic", m.Topic)
		if m.Topic == contract.ConfigSetTopic(a.cfg.Site) {
			a.refuseBusy(m)
		}
	}
}

// refuseBusy tells the cloud that a configuration the broker acknowledged was dropped:
// never lost in silence. Its id is not marked seen, so the cloud may send it again.
func (a *App) refuseBusy(m inbox.Message) {
	env, err := inbox.Envelope(m, a.cfg.Site, contract.TypeConfigSet)
	if err != nil {
		return
	}
	d, err := contract.DecodeData[contract.ConfigSetData](env)
	if err != nil {
		return
	}
	a.answer(a.deps.Clock.Now(), d.Version, "the hub is busy: send it again")
}

// alarmState follows the alarm's state. A retained state comes again at each
// connection: no duplicate to drop.
func (a *App) alarmState(ctx context.Context, m inbox.Message) {
	_, d, err := inbox.Open[contract.AlarmStateData](m, a.cfg.Site, contract.TypeAlarmState)
	if err != nil {
		a.log.Warn("ignoring an invalid alarm state", "err", err)
		return
	}
	a.mu.Lock()
	a.state = d.State
	a.mu.Unlock()
	a.restartIfQuiet(ctx)
}

// installMode follows the installation mode. The hub publishes it on the same outbox
// as the alarm's state, in order: a mode closed by an arming arrives after the
// arming's state, so the exit delay is known before the mode's end.
func (a *App) installMode(ctx context.Context, m inbox.Message) {
	_, d, err := inbox.Open[contract.InstallModeData](m, a.cfg.Site, contract.TypeInstallMode)
	if err != nil {
		a.log.Warn("ignoring an invalid installation mode", "err", err)
		return
	}
	a.mu.Lock()
	a.installing = d.Status == contract.InstallOpen
	a.mu.Unlock()
	a.restartIfQuiet(ctx)
}

// configSet takes a configuration. Its data is checked only after its version: the
// cloud is told why a configuration is refused, and an old one is refused as old.
func (a *App) configSet(ctx context.Context, m inbox.Message) {
	now := a.deps.Clock.Now()
	env, err := inbox.Envelope(m, a.cfg.Site, contract.TypeConfigSet)
	if err != nil {
		a.log.Warn("ignoring an invalid configuration message", "err", err)
		return
	}
	if !a.seen.FirstTime(env.ID) {
		return
	}
	d, err := contract.DecodeData[contract.ConfigSetData](env)
	if err != nil {
		a.answer(now, 0, "damaged configuration")
		return
	}
	a.mu.Lock()
	current := max(a.cfg.Running, a.saved)
	a.mu.Unlock()
	switch {
	case d.Version != 0 && d.Version < current:
		a.answer(now, d.Version, fmt.Sprintf("older than version %d", current))
		return
	case d.Version == a.cfg.Running && a.cfg.Running > 0:
		a.answer(now, d.Version, "") // the cloud asks again: it is the one running
		return
	case d.Version != 0 && d.Version == current:
		return // kept already, waiting for a quiet moment
	}
	if err := d.Validate(); err != nil {
		a.log.Warn("refused an invalid configuration from the cloud", "version", d.Version, "err", err) // no phone in it
		a.answer(now, d.Version, err.Error())
		return
	}
	if err := a.deps.Check(d); err != nil {
		a.log.Warn("refused a configuration from the cloud", "version", d.Version, "err", err)
		a.answer(now, d.Version, err.Error())
		return
	}
	if err := a.deps.Save(d); err != nil {
		a.log.Error("could not keep the configuration from the cloud", "version", d.Version, "err", err)
		a.answer(now, d.Version, "the hub could not keep it")
		return
	}
	a.log.Info("configuration from the cloud kept, the hub restarts to apply it at a quiet moment", "version", d.Version)
	a.mu.Lock()
	a.saved = d.Version
	a.mu.Unlock()
	a.restartIfQuiet(ctx)
}

// restartIfQuiet restarts the hub when a configuration or new devices wait and the
// moment is quiet (see QuietState), and no installation is under way. The state said on
// the broker must be quiet, and the engine must say so too: its states reach the broker
// behind it, and a delay begun a moment ago is not said yet. Its next state asks again.
func (a *App) restartIfQuiet(ctx context.Context) {
	devices := a.deps.DevicesChanged != nil && a.deps.DevicesChanged() // reads a file: outside the lock
	a.mu.Lock()
	pending := (a.saved != 0 || devices) && !a.restarted
	said := QuietState(a.state, devices) && !a.installing
	version, state, installing := a.saved, a.state, a.installing
	a.mu.Unlock()
	if !pending {
		return
	}
	if said && ctx.Err() == nil && a.engineQuiet(ctx, devices) {
		a.mu.Lock()
		a.restarted = true
		a.mu.Unlock()
		a.log.Info("restarting to apply the new configuration", "version", version, "devices", devices)
		a.deps.Restart()
		return
	}
	a.log.Info("waiting for a quiet moment to restart", "version", version, "devices", devices, "alarm", state, "installing", installing)
}

func (a *App) engineQuiet(ctx context.Context, devices bool) bool {
	ctx, cancel := context.WithTimeout(ctx, quietWait)
	defer cancel()
	return a.deps.Quiet(ctx, func(state string) bool { return QuietState(state, devices) })
}

// QuietState tells whether the alarm's state (contract.State*) lets the hub restart:
// disarmed or armed, never in the middle of a delay or of an alarm. When the zones or
// the sirens changed (devices), the exit delay is quiet and armed is not: an arming
// made on the old zones never saw the new devices, and the restart once armed would take
// a window of a new zone left open for one opened while the hub was off, a siren the
// moment the shop arms. Restarted during the exit delay, the hub ends it on the new
// zones: that window is bypassed and told, as at the end of any exit delay. New devices
// found armed wait for the next disarm.
func QuietState(state string, devices bool) bool {
	switch state {
	case contract.StateDisarmed:
		return true
	case contract.StateArmed:
		return !devices
	case contract.StateArming:
		return devices
	}
	return false
}

// answer publishes config.applied: applied when why is empty, rejected otherwise.
func (a *App) answer(now time.Time, version int, why string) {
	d := contract.ConfigAppliedData{Version: version, Status: contract.ConfigApplied}
	if why != "" {
		d.Status, d.Reason = contract.ConfigRejected, why
	}
	a.out.Send(src, contract.ConfigAppliedTopic(a.cfg.Site), contract.TypeConfigApplied, d, true, now)
}
