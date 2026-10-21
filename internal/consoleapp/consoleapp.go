// Package consoleapp is the application of the local console (docs/security-install.md):
// a web page the installer opens on a phone, on the hub's own Wi-Fi network, to pair
// the devices, give each its use, test them, and close the installation mode.
//
// The console lives only while the installation mode lasts, which the hub opens when
// the installer code is typed at the keypad (install.mode, retained). Then it turns
// on the network and serves the page; when the mode ends, it stops both. Its work on
// the devices is the installer's application (internal/install), the same as the
// command-line tool's.
//
// Everything outside is a port: the network (Hotspot), the zones and sirens of the
// hub (Sites), the web server (Serve), the broker (Publisher, and the messages given
// to HandleMessage). The web page itself is in package web, on top of this one.
package consoleapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/install"
)

// Source names the console in the envelopes it sends.
const Source = "console"

// Config holds what the console needs to know.
type Config struct {
	Site      string
	PairFor   time.Duration // how long a pairing window stays open (2 min)
	MaxEvents int           // device events kept for the tests page (50)
	Timing    install.Timing
	// Wait bounds the network's start and stop, and an order the page waits for.
	Wait time.Duration
}

// DefaultConfig returns the defaults.
func DefaultConfig(site string) Config {
	return Config{Site: site, PairFor: 2 * time.Minute, MaxEvents: 50, Wait: 30 * time.Second}
}

// Hotspot is the hub's own Wi-Fi network.
type Hotspot interface {
	Up(ctx context.Context) error
	Down(ctx context.Context) error
}

// Sites keeps the zones and the sirens of the hub; the console changes them through
// the installer's application (install.Editor), as the command-line tool does.
type Sites = install.Sites

// Person is a contact of the site, and whether they have an access code.
type Person struct {
	ID, Name, Role string
	HasCode        bool
}

// Codes are the people's access codes, kept by the hub. People lists the site's
// contacts, and apart the users of codes who are no longer contacts (leftovers: the
// hub leaves their codes aside, the installer removes them). Set refuses a code that
// is malformed, too easy, someone else's or the installer's (the ErrCode errors);
// Remove refuses a user without a code (ErrNoSuchCode). Both have the running hub read
// the codes again. Nothing here ever logs a code.
type Codes interface {
	People() (contacts []Person, leftovers []string, err error)
	Set(user, code string) error
	Remove(user string) error
}

// Deps are the ports of the console.
type Deps struct {
	Clock     driver.Clock
	Publisher install.Publisher
	NewID     func() string
	Hotspot   Hotspot
	Sites     Sites
	Codes     Codes
	// Serve serves the web page until ctx ends: on the hub's address on its own
	// network, which exists only once the hotspot is up.
	Serve  func(ctx context.Context) error
	Logger *slog.Logger
}

// Message is a message received from the broker.
type Message struct {
	Topic   string
	Payload []byte
	Retain  bool
}

// The console's refusals, for the page to explain.
var (
	ErrClosed     = errors.New("the installation mode is closed")
	ErrBusy       = errors.New("a pairing is under way: wait for its end")
	ErrUnknown    = errors.New("no such device among the paired ones")
	ErrNoRegistry = errors.New("the devices are not known yet: is safehome-zigbee running?")

	// The installer's application's refusals, named here for the page.
	ErrNotASiren   = install.ErrCannotSound
	ErrNoZone      = install.ErrNoZone
	ErrLastSiren   = install.ErrLastSiren
	ErrEmptyName   = install.ErrEmptyName
	ErrNameTooLong = install.ErrNameTooLong
	ErrCannotServe = install.ErrCannotServe
	ErrNoAnswer    = install.ErrNoAnswer

	ErrNotAContact  = errors.New("no such person among the site's contacts")
	ErrCodeMismatch = errors.New("the two codes differ")
	ErrCodeInvalid  = errors.New("a code is 4 to 8 digits")
	ErrCodeWeak     = errors.New("code too easy to guess")
	ErrCodeTaken    = errors.New("this code belongs to someone else")
	ErrCodeFull     = errors.New("the site already has the maximum number of codes")
	ErrNoSuchCode   = errors.New("no code left by someone who is no longer a contact")
)

// App is the console.
type App struct {
	cfg  Config
	deps Deps
	log  *slog.Logger
	sess *install.Session
	site *install.Editor

	modes chan contract.InstallModeData
	jobs  chan func(context.Context)
	ping  chan chan struct{}

	mu         sync.Mutex
	open       bool
	opened     time.Time // the tests page shows the events from then on
	until      time.Time
	view       install.View
	known      bool
	events     []Event
	pairing    *Pairing // the current mode's latest window, nil before the first
	busy       bool
	session    context.Context // the current installation mode's; nil when closed
	hotspotErr string          // why the network did not start, if it did not
}

// New builds the console.
func New(cfg Config, deps Deps) (*App, error) {
	if !contract.ValidSite(cfg.Site) {
		return nil, fmt.Errorf("consoleapp: invalid site %q", cfg.Site)
	}
	if cfg.PairFor <= 0 || cfg.MaxEvents < 1 || cfg.Wait <= 0 {
		return nil, errors.New("consoleapp: PairFor, MaxEvents and Wait must be positive")
	}
	if deps.Clock == nil || deps.Publisher == nil || deps.NewID == nil || deps.Hotspot == nil || deps.Sites == nil || deps.Codes == nil || deps.Serve == nil {
		return nil, errors.New("consoleapp: Clock, Publisher, NewID, Hotspot, Sites, Codes and Serve are required")
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	sess, err := install.NewSession(cfg.Site, install.Deps{Publisher: deps.Publisher, Now: deps.Clock.Now, NewID: deps.NewID}, cfg.Timing)
	if err != nil {
		return nil, err
	}
	return &App{
		cfg: cfg, deps: deps, log: log, sess: sess, site: install.NewEditor(deps.Sites),
		modes: make(chan contract.InstallModeData, 16),
		jobs:  make(chan func(context.Context), 1),
		ping:  make(chan chan struct{}),
	}, nil
}

// Subscription is a topic filter the console listens to.
type Subscription = install.Subscription

// Subscriptions lists what the program must subscribe to.
func (a *App) Subscriptions() []Subscription {
	return append(a.sess.Subscriptions(), Subscription{Filter: contract.InstallModeTopic(a.cfg.Site), QoS: 1})
}

// HandleMessage takes a message from the broker. It never blocks.
func (a *App) HandleMessage(m Message) {
	if m.Topic != contract.InstallModeTopic(a.cfg.Site) {
		a.sess.Deliver(install.Message{Topic: m.Topic, Payload: m.Payload, Retain: m.Retain})
		return
	}
	env, err := contract.ParseEnvelope(m.Payload)
	if err != nil || env.Site != a.cfg.Site || env.Type != contract.TypeInstallMode {
		return
	}
	d, err := contract.DecodeData[contract.InstallModeData](env)
	if err != nil || d.Validate() != nil {
		a.log.Warn("ignoring an invalid installation mode")
		return
	}
	select {
	case a.modes <- d:
	default:
		a.log.Warn("too many installation mode messages, dropping one")
	}
}

// Alive returns once the console's loop answers: the program checks it before telling
// systemd that the console still works.
func (a *App) Alive(ctx context.Context) error {
	reply := make(chan struct{})
	select {
	case a.ping <- reply:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
