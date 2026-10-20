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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

// Sites keeps the zones and the sirens of the hub. Write and Check refuse what the hub
// would refuse.
type Sites interface {
	Read() (install.Site, error)
	Check(install.Site) error
	Write(install.Site) error
}

// Person is a contact of the site, and whether they have an access code.
type Person struct {
	ID, Name, Role string
	HasCode        bool
}

// Codes are the people's access codes, kept by the hub. Set refuses a code that is
// malformed, too easy, someone else's or the installer's (the ErrCode errors), and
// has the running hub read the codes again. Nothing here ever logs a code.
type Codes interface {
	People() ([]Person, error)
	Set(user, code string) error
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
	ErrNotASiren  = errors.New("this device cannot sound")
	ErrNoRegistry = errors.New("the devices are not known yet: is safehome-zigbee running?")

	ErrNotAContact  = errors.New("no such person among the site's contacts")
	ErrCodeMismatch = errors.New("the two codes differ")
	ErrCodeInvalid  = errors.New("a code is 4 to 8 digits")
	ErrCodeWeak     = errors.New("code too easy to guess")
	ErrCodeTaken    = errors.New("this code belongs to someone else")
	ErrCodeFull     = errors.New("the site already has the maximum number of codes")
)

// App is the console.
type App struct {
	cfg  Config
	deps Deps
	log  *slog.Logger
	sess *install.Session

	modes chan contract.InstallModeData
	jobs  chan func(context.Context)
	ping  chan chan struct{}

	mu       sync.Mutex
	open     bool
	opened   time.Time // the tests page shows the events from then on
	until    time.Time
	view     install.View
	known    bool
	events   []Event
	pairing  *Pairing
	busy     bool
	session  context.Context // the current installation mode's; nil when closed
	hotspotE string          // why the network did not start, if it did not
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
		cfg: cfg, deps: deps, log: log, sess: sess,
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

// Run serves the console until ctx is cancelled.
func (a *App) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); a.worker(ctx) }()
	defer wg.Wait()

	var stop func() // ends the current installation mode's network and page
	var deadline <-chan time.Time
	var cancelTimer func()
	end := func(why string) {
		if stop == nil {
			return
		}
		a.log.Info("installation mode over: the network and the page stop", "reason", why)
		cancelTimer()
		stop()
		stop, deadline = nil, nil
	}
	defer end("the console stops")
	for {
		select {
		case <-ctx.Done():
			return
		case reply := <-a.ping:
			close(reply)
		case <-deadline:
			end(contract.InstallTimeout) // the hub says so too, unless it is gone
		case d := <-a.modes:
			switch {
			case d.Status == contract.InstallClosed:
				end(d.Reason)
			case stop == nil:
				until := d.Until.Time
				left := until.Sub(a.deps.Clock.Now())
				if left <= 0 {
					continue // an old message: the mode is over already
				}
				stop = a.start(ctx, until)
				deadline, cancelTimer = a.deps.Clock.Timer(left)
			}
		}
	}
}

// start turns on the network and the page for an installation mode, and returns how
// to stop them.
func (a *App) start(ctx context.Context, until time.Time) func() {
	sctx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.open, a.opened, a.until, a.session = true, a.deps.Clock.Now(), until, sctx
	a.events, a.pairing, a.hotspotE = nil, nil, ""
	a.mu.Unlock()
	a.log.Warn("installation mode open: the network and the page start", "until", until.Format(time.RFC3339))

	upCtx, upDone := context.WithTimeout(sctx, a.cfg.Wait)
	err := a.deps.Hotspot.Up(upCtx)
	upDone()
	var served sync.WaitGroup
	if err != nil {
		a.log.Error("the console's network did not start: the page cannot be reached", "err", err)
		a.mu.Lock()
		a.hotspotE = err.Error()
		a.mu.Unlock()
	} else {
		served.Add(1)
		go func() {
			defer served.Done()
			if err := a.deps.Serve(sctx); err != nil && sctx.Err() == nil {
				a.log.Error("the console's page stopped", "err", err)
			}
		}()
	}
	return func() {
		cancel()
		served.Wait()
		if err == nil {
			downCtx, downDone := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.Wait)
			if err := a.deps.Hotspot.Down(downCtx); err != nil {
				a.log.Error("the console's network may still be on", "err", err)
			}
			downDone()
		}
		a.mu.Lock()
		a.open, a.session = false, nil
		a.mu.Unlock()
	}
}

// worker owns the installer's session: it follows the broker, and runs the orders of
// the page (a pairing, a removal) one at a time.
func (a *App) worker(ctx context.Context) {
	for {
		var next func(context.Context)
		fctx, cancel := context.WithCancel(ctx)
		got := make(chan struct{})
		go func() {
			defer close(got)
			select {
			case next = <-a.jobs:
				cancel()
			case <-fctx.Done():
			}
		}()
		a.sess.Follow(fctx, a.setView, a.addEvent)
		cancel()
		<-got
		if ctx.Err() != nil {
			return
		}
		if next != nil {
			next(ctx)
			if v, ok := a.sess.Current(); ok {
				a.setView(v)
			}
		}
	}
}

func (a *App) setView(v install.View) {
	a.mu.Lock()
	a.view, a.known = v, true
	a.mu.Unlock()
}

func (a *App) addEvent(at time.Time, e contract.DeviceEventData) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.open || at.Before(a.opened) {
		return // from before the mode: the worker may read it late
	}
	a.events = append(a.events, Event{At: at, DeviceID: e.DeviceID, Capability: e.Capability, Value: e.StringValue()})
	if len(a.events) > a.cfg.MaxEvents {
		a.events = a.events[len(a.events)-a.cfg.MaxEvents:]
	}
}

// --- what the page shows ------------------------------------------------------------

// Overview is what the page shows.
type Overview struct {
	Open     bool
	Until    time.Time
	Hotspot  string // why the network did not start
	Known    bool   // the registry arrived
	Status   *contract.ZigbeeStatusData
	Devices  []Device
	Findings []install.Finding
	SiteErr  string
	Pairing  *Pairing
	Busy     bool    // an order runs: a pairing, a removal
	Events   []Event // the latest first
	People   []Person
	CodesErr string
}

// Device is one paired device, or one the site uses without it being paired.
type Device struct {
	ID           string
	Model        string
	Capabilities []string
	Paired       bool
	Offline      bool
	Zone         *install.Zone
	Siren        bool
	CanSound     bool
	Keypad       bool
	Kinds        []string // the kinds of zone it can serve
}

// Event is a device event, for the tests.
type Event struct {
	At         time.Time
	DeviceID   string
	Zone       string // the zone's name, if any
	Capability string
	Value      string
}

// Pairing is the current or last pairing window.
type Pairing struct {
	Active   bool
	Until    time.Time
	Added    []contract.RegistryDeviceAddedData
	Rejected []contract.RegistryDeviceRejectedData
	Err      string
}

// Overview returns what the page shows now.
func (a *App) Overview() Overview {
	site, siteErr := a.deps.Sites.Read()
	people, codesErr := a.deps.Codes.People()
	a.mu.Lock()
	o := Overview{Open: a.open, Until: a.until, Hotspot: a.hotspotE, Known: a.known, Status: a.view.Status, Busy: a.busy}
	devices := slices.Clone(a.view.Devices)
	events := slices.Clone(a.events)
	if a.pairing != nil {
		p := *a.pairing
		p.Added, p.Rejected = slices.Clone(p.Added), slices.Clone(p.Rejected)
		o.Pairing = &p
	}
	a.mu.Unlock()

	o.People = people
	if codesErr != nil {
		o.CodesErr = codesErr.Error()
	}
	if siteErr != nil {
		o.SiteErr = siteErr.Error()
		site = install.Site{Zones: map[string]install.Zone{}}
	}
	offline := map[string]bool{}
	if o.Status != nil {
		for _, id := range o.Status.Offline {
			offline[id] = true
		}
	}
	seen := map[string]bool{}
	for _, d := range devices {
		seen[d.DeviceID] = true
		o.Devices = append(o.Devices, a.device(site, d.DeviceID, d.Model, d.Capabilities, true, offline[d.DeviceID]))
	}
	// What the site uses without it being paired: a mistake to show, and to undo.
	for id := range site.Zones {
		if !seen[id] {
			seen[id] = true
			o.Devices = append(o.Devices, a.device(site, id, "", nil, false, false))
		}
	}
	for _, id := range site.SirenIDs {
		if !seen[id] {
			seen[id] = true
			o.Devices = append(o.Devices, a.device(site, id, "", nil, false, false))
		}
	}
	slices.SortFunc(o.Devices, func(x, y Device) int {
		if x.ID < y.ID {
			return -1
		}
		if x.ID > y.ID {
			return 1
		}
		return 0
	})
	if o.Known && siteErr == nil {
		o.Findings = install.Check(site, devices)
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if z, ok := site.Zones[e.DeviceID]; ok {
			e.Zone = z.Name
		}
		o.Events = append(o.Events, e)
	}
	return o
}

func (a *App) device(site install.Site, id, model string, caps []string, paired, offline bool) Device {
	d := Device{ID: id, Model: model, Capabilities: caps, Paired: paired, Offline: offline,
		Siren: slices.Contains(site.SirenIDs, id), CanSound: slices.Contains(caps, contract.CapSiren),
		Keypad: slices.Contains(caps, contract.CapKeypad)}
	if z, ok := site.Zones[id]; ok {
		d.Zone = &z
	}
	for _, k := range install.ZoneKinds() {
		if install.CanServe(k, caps) == nil {
			d.Kinds = append(d.Kinds, k)
		}
	}
	return d
}

// --- what the page asks -------------------------------------------------------------

// sessionCtx returns the current installation mode's context, or ErrClosed.
func (a *App) sessionCtx() (context.Context, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.open || a.session == nil {
		return nil, ErrClosed
	}
	return a.session, nil
}

// submit hands an order to the worker; only one at a time.
func (a *App) submit(job func(context.Context)) error {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return ErrBusy
	}
	a.busy = true
	a.mu.Unlock()
	done := func(ctx context.Context) {
		defer func() { a.mu.Lock(); a.busy = false; a.mu.Unlock() }()
		job(ctx)
	}
	select {
	case a.jobs <- done:
		return nil
	default:
		a.mu.Lock()
		a.busy = false
		a.mu.Unlock()
		return ErrBusy
	}
}

// Pair opens the Zigbee network for PairFor. It returns at once; the page shows the
// devices as they join.
func (a *App) Pair() error {
	sctx, err := a.sessionCtx()
	if err != nil {
		return err
	}
	return a.submit(func(context.Context) {
		until := a.deps.Clock.Now().Add(a.cfg.PairFor)
		a.mu.Lock()
		a.pairing = &Pairing{Active: true, Until: until}
		a.mu.Unlock()
		a.log.Info("pairing opened from the console", "for", a.cfg.PairFor)
		err := a.sess.Pair(sctx, a.cfg.PairFor, func(n install.PairNews) {
			a.mu.Lock()
			defer a.mu.Unlock()
			switch {
			case n.Added != nil:
				a.pairing.Added = append(a.pairing.Added, *n.Added)
			case n.Rejected != nil:
				a.pairing.Rejected = append(a.pairing.Rejected, *n.Rejected)
			}
		})
		a.mu.Lock()
		a.pairing.Active = false
		if err != nil {
			a.pairing.Err = err.Error()
		}
		added := len(a.pairing.Added)
		a.mu.Unlock()
		a.log.Info("pairing over", "added", added, "err", err)
	})
}

// paired returns the capabilities of a paired device.
func (a *App) paired(id string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.known {
		return nil, ErrNoRegistry
	}
	for _, d := range a.view.Devices {
		if d.DeviceID == id {
			return d.Capabilities, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknown, id)
}

// change reads the site, applies edit, and keeps the result if the hub would take it.
func (a *App) change(edit func(*install.Site) error) error {
	if _, err := a.sessionCtx(); err != nil {
		return err
	}
	site, err := a.deps.Sites.Read()
	if err != nil {
		return err
	}
	if site.Zones == nil {
		site.Zones = map[string]install.Zone{}
	}
	if err := edit(&site); err != nil {
		return err
	}
	return a.deps.Sites.Write(site)
}

// SetZone gives a paired device its zone.
func (a *App) SetZone(id, name, kind string) error {
	caps, err := a.paired(id)
	if err != nil {
		return err
	}
	if err := install.ValidName(name); err != nil {
		return err
	}
	if err := install.CanServe(kind, caps); err != nil {
		return err
	}
	err = a.change(func(s *install.Site) error {
		s.Zones[id] = install.Zone{Name: name, Kind: kind}
		return nil
	})
	if err == nil {
		a.log.Info("zone set from the console", "device", id, "kind", kind)
	}
	return err
}

// Unzone takes a device out of its zone.
func (a *App) Unzone(id string) error {
	err := a.change(func(s *install.Site) error {
		if _, ok := s.Zones[id]; !ok {
			return fmt.Errorf("%s has no zone", id)
		}
		delete(s.Zones, id)
		return nil
	})
	if err == nil {
		a.log.Info("zone removed from the console", "device", id)
	}
	return err
}

// Siren makes a device a siren, or stops. Only a paired device that can sound becomes
// one; any can stop.
func (a *App) Siren(id string, on bool) error {
	if on {
		caps, err := a.paired(id)
		if err != nil {
			return err
		}
		if !slices.Contains(caps, contract.CapSiren) {
			return ErrNotASiren
		}
	}
	err := a.change(func(s *install.Site) error {
		has := slices.Contains(s.SirenIDs, id)
		switch {
		case on && !has:
			s.SirenIDs = append(s.SirenIDs, id)
		case !on && has:
			s.SirenIDs = slices.DeleteFunc(s.SirenIDs, func(x string) bool { return x == id })
		}
		return nil
	})
	if err == nil {
		a.log.Info("siren changed from the console", "device", id, "siren", on)
	}
	return err
}

// Remove takes a device out of the network for good, then out of the site. It is
// refused before anything is sent if the site without it would not work (the last
// siren). It waits for the Zigbee adapter's answer.
func (a *App) Remove(ctx context.Context, id string) error {
	sctx, err := a.sessionCtx()
	if err != nil {
		return err
	}
	if _, err := a.paired(id); err != nil {
		return err
	}
	site, err := a.deps.Sites.Read()
	if err != nil {
		return err
	}
	without := install.Site{Zones: map[string]install.Zone{}, SirenIDs: slices.DeleteFunc(slices.Clone(site.SirenIDs), func(x string) bool { return x == id })}
	for k, z := range site.Zones {
		if k != id {
			without.Zones[k] = z
		}
	}
	if err := a.deps.Sites.Check(without); err != nil {
		return fmt.Errorf("cannot be removed yet: %w", err)
	}
	result := make(chan error, 1)
	if err := a.submit(func(context.Context) { result <- a.sess.Remove(sctx, id) }); err != nil {
		return err
	}
	select {
	case err = <-result:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	a.log.Info("device removed from the console", "device", id)
	return a.change(func(s *install.Site) error {
		delete(s.Zones, id)
		s.SirenIDs = slices.DeleteFunc(s.SirenIDs, func(x string) bool { return x == id })
		return nil
	})
}

// SetCode sets the access code of a contact of the site, typed twice by that person
// on the spot. The code is never logged nor kept here.
func (a *App) SetCode(user, code, again string) error {
	if _, err := a.sessionCtx(); err != nil {
		return err
	}
	if code != again {
		return ErrCodeMismatch
	}
	people, err := a.deps.Codes.People()
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(people, func(p Person) bool { return p.ID == user }) {
		return fmt.Errorf("%w: %s", ErrNotAContact, user)
	}
	if err := a.deps.Codes.Set(user, code); err != nil {
		return err
	}
	a.log.Info("access code set from the console", "user", user)
	return nil
}

// Finish asks the hub to close the installation mode: the console stops when the hub
// says it is closed, and the hub restarts if the zones or the sirens changed.
func (a *App) Finish(ctx context.Context) error {
	if _, err := a.sessionCtx(); err != nil {
		return err
	}
	now := a.deps.Clock.Now()
	env, err := contract.NewEnvelope(a.cfg.Site, Source, uint64(now.UnixMilli()), a.deps.NewID(), now, contract.TypeInstallClose, contract.InstallCloseData{})
	if err != nil {
		return err
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, a.cfg.Wait)
	defer cancel()
	if err := a.deps.Publisher.Publish(pctx, contract.InstallCmdTopic(a.cfg.Site), payload, 1, false); err != nil {
		return fmt.Errorf("the hub could not be told: %w", err)
	}
	a.log.Info("the installer closed the installation mode from the console")
	return nil
}
