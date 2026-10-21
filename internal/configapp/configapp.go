// Package configapp applies the site's configuration set from the cloud
// (config.set, see docs/contract.md). It runs in the hub's program: it checks a new
// configuration against the site's files, keeps it in the state folder, and has the
// hub restart to load it, at a quiet moment: never during an exit or entry delay, an
// alarm, nor the installation mode. It answers with config.applied, retained, once the
// hub runs with it, or at once when it refuses it. The zones and sirens changed by the
// installer's tools are applied the same way.
//
// The other programs that read this configuration (the GSM gateway, the video
// service) restart when config.applied names a version they do not run.
//
// Checking, keeping and restarting are ports, wired by the composition root
// (cmd/safehome-hub): this package knows no file and no process.
package configapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver"
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

	OutboxSize int
	RetryMin   time.Duration
	RetryMax   time.Duration
	DedupeSize int
}

// DefaultConfig returns the defaults.
func DefaultConfig(site string, running int) Config {
	return Config{Site: site, Running: running, OutboxSize: 50, RetryMin: 200 * time.Millisecond, RetryMax: 5 * time.Second, DedupeSize: 64}
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
	// DevicesChanged says whether the installer's tools (the local console, the
	// command-line tool) changed the zones or the sirens since the hub loaded them: the
	// hub restarts for them too. Asked at each quiet moment; nil when none can change.
	DevicesChanged func() bool
	// Logger: never given a configuration, which holds phone numbers.
	Logger *slog.Logger
}

// Message is a message received from the broker.
type Message struct {
	Topic   string
	Payload []byte
}

// Subscription is a topic filter the application listens to.
type Subscription struct {
	Filter string
	QoS    byte
}

const (
	src        = "config"
	keySeq     = "config_seq"
	maxPayload = 64 * 1024
)

// App is the application.
type App struct {
	cfg  Config
	deps Deps
	log  *slog.Logger
	out  *outbox.Outbox
	seen *outbox.RecentIDs

	mu         sync.Mutex
	saved      int    // the version kept, waiting for a quiet moment (0: none)
	state      string // the alarm's state, from alarm/state ("" until known)
	installing bool   // the installation mode is open, from install/mode
	restarted  bool
}

// New builds the application.
func New(cfg Config, deps Deps) (*App, error) {
	if !contract.ValidSite(cfg.Site) {
		return nil, fmt.Errorf("configapp: invalid site %q", cfg.Site)
	}
	if cfg.OutboxSize < 1 || cfg.RetryMin <= 0 || cfg.RetryMax < cfg.RetryMin || cfg.DedupeSize < 1 {
		return nil, errors.New("configapp: OutboxSize, RetryMin, RetryMax and DedupeSize must be set")
	}
	if deps.Clock == nil || deps.Store == nil || deps.Publisher == nil || deps.NewID == nil ||
		deps.Check == nil || deps.Save == nil || deps.Restart == nil {
		return nil, errors.New("configapp: Clock, Store, Publisher, NewID, Check, Save and Restart are required")
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	seq, err := outbox.NewSequencer(deps.Store, keySeq, log)
	if err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, deps: deps, log: log, seen: outbox.NewRecentIDs(cfg.DedupeSize)}
	a.out = outbox.New(deps.Publisher, seq, cfg.Site, cfg.OutboxSize, cfg.RetryMin, cfg.RetryMax, log)
	return a, nil
}

// Subscriptions lists what the application listens to: the configuration, and the
// alarm's state and the installation mode, to restart at a quiet moment.
func (a *App) Subscriptions() []Subscription {
	return []Subscription{
		{Filter: contract.ConfigSetTopic(a.cfg.Site), QoS: 1},
		{Filter: contract.AlarmStateTopic(a.cfg.Site), QoS: 1},
		{Filter: contract.InstallModeTopic(a.cfg.Site), QoS: 1},
	}
}

// Run serves the application until ctx is cancelled. At start it says which
// configuration the hub runs with, or which one it could not load.
func (a *App) Run(ctx context.Context) {
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
	a.out.Run(ctx)
	a.out.Flush(3 * time.Second)
}

// HandleMessage takes a message from the broker. It returns quickly.
func (a *App) HandleMessage(m Message) {
	if len(m.Payload) > maxPayload {
		return
	}
	switch m.Topic {
	case contract.AlarmStateTopic(a.cfg.Site):
		a.alarmState(m.Payload)
	case contract.InstallModeTopic(a.cfg.Site):
		a.installMode(m.Payload)
	case contract.ConfigSetTopic(a.cfg.Site):
		a.configSet(m.Payload)
	}
}

func (a *App) alarmState(payload []byte) {
	env, err := contract.ParseEnvelope(payload)
	if err != nil || env.Site != a.cfg.Site || env.Type != contract.TypeAlarmState {
		return
	}
	d, err := contract.DecodeData[contract.AlarmStateData](env)
	if err != nil {
		return
	}
	a.mu.Lock()
	a.state = d.State
	a.mu.Unlock()
	a.restartIfQuiet()
}

// installMode follows the installation mode. The hub publishes it on the same outbox
// as the alarm's state, in order: a mode closed by an arming arrives after the
// arming's state, so the exit delay is known before the mode's end.
func (a *App) installMode(payload []byte) {
	env, err := contract.ParseEnvelope(payload)
	if err != nil || env.Site != a.cfg.Site || env.Type != contract.TypeInstallMode {
		return
	}
	d, err := contract.DecodeData[contract.InstallModeData](env)
	if err != nil || d.Validate() != nil {
		return
	}
	a.mu.Lock()
	a.installing = d.Status == contract.InstallOpen
	a.mu.Unlock()
	a.restartIfQuiet()
}

func (a *App) configSet(payload []byte) {
	now := a.deps.Clock.Now()
	env, err := contract.ParseEnvelope(payload)
	if err != nil || env.Site != a.cfg.Site || env.Type != contract.TypeConfigSet {
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
	a.restartIfQuiet()
}

// restartIfQuiet restarts the hub when a configuration or new devices wait and the
// moment is quiet: the alarm disarmed or armed, never in the middle of a delay or of
// an alarm, and no installation under way.
func (a *App) restartIfQuiet() {
	devices := a.deps.DevicesChanged != nil && a.deps.DevicesChanged() // reads a file: outside the lock
	a.mu.Lock()
	pending := (a.saved != 0 || devices) && !a.restarted
	quiet := (a.state == contract.StateDisarmed || a.state == contract.StateArmed) && !a.installing
	if pending && quiet {
		a.restarted = true
	}
	version, state, installing := a.saved, a.state, a.installing
	a.mu.Unlock()
	switch {
	case pending && quiet:
		a.log.Info("restarting to apply the new configuration", "version", version, "devices", devices)
		a.deps.Restart()
	case pending:
		a.log.Info("waiting for a quiet moment to restart", "version", version, "devices", devices, "alarm", state, "installing", installing)
	}
}

// answer publishes config.applied: applied when why is empty, rejected otherwise.
func (a *App) answer(now time.Time, version int, why string) {
	d := contract.ConfigAppliedData{Version: version, Status: contract.ConfigApplied}
	if why != "" {
		d.Status, d.Reason = contract.ConfigRejected, why
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return
	}
	a.out.Put(&outbox.Item{
		Topic: contract.ConfigAppliedTopic(a.cfg.Site), Src: src, Type: contract.TypeConfigApplied,
		Data: raw, QoS: 1, Retain: true, ID: a.deps.NewID(), TS: now,
	})
}
