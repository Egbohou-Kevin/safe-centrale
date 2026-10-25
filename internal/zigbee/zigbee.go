// Package zigbee is the domain core of the Zigbee adapter (see
// docs/adapter-zigbee.md): it turns what the devices report into capability changes,
// watches that each device still answers, drives commands until the device confirms
// them, and keeps the registry through pairing and removal.
//
// It is deterministic: it does not read the clock, write anywhere or talk to any
// hardware. Inputs carry their own time; actions are data for a driver. It knows the
// shape of Zigbee2MQTT's data only as maps of fields, read through profiles; the
// z2m subpackage parses and builds Zigbee2MQTT's messages, the wire subpackage
// translates to the event contract. An architecture test (internal/archtest)
// enforces this.
//
// A Network is not safe for concurrent use: the driver feeds it one input at a time.
package zigbee

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

// Config holds the settings of the adapter.
type Config struct {
	Profiles []Profile
	// ConfirmWait is how long a command waits for the device's confirmation before
	// it is sent again, Attempts times in all.
	ConfirmWait time.Duration
	Attempts    int
	// OfflineAfter is the silence after which a device counts as offline, for the
	// profiles that do not say (docs/engine.md: 4 hours).
	OfflineAfter time.Duration
	// PairMax bounds how long the network may stay open to new devices.
	PairMax time.Duration
	// Step is how much a number (battery, link) must move to be announced.
	Step int
}

// DefaultConfig returns the defaults of docs/adapter-zigbee.md.
func DefaultConfig(profiles []Profile) Config {
	return Config{
		Profiles: profiles, ConfirmWait: 3 * time.Second, Attempts: 3,
		OfflineAfter: 4 * time.Hour, PairMax: 10 * time.Minute, Step: 5,
	}
}

// Validate checks the configuration, profiles included.
func (c Config) Validate() error {
	if c.ConfirmWait <= 0 || c.Attempts < 1 || c.OfflineAfter <= 0 || c.PairMax <= 0 || c.Step < 1 {
		return fmt.Errorf("zigbee: ConfirmWait, Attempts, OfflineAfter, PairMax and Step must be positive")
	}
	names, models := map[string]bool{}, map[string]string{}
	for _, p := range c.Profiles {
		if err := p.validate(); err != nil {
			return err
		}
		if names[p.Name] {
			return fmt.Errorf("zigbee: two profiles are named %s", p.Name)
		}
		names[p.Name] = true
		for _, m := range p.Models {
			if other, ok := models[m]; ok {
				return fmt.Errorf("zigbee: model %s is in profiles %s and %s", m, other, p.Name)
			}
			models[m] = p.Name
		}
	}
	return nil
}

// Device is a registered device: what it is. What it is for (name, zone) belongs to
// the site's configuration.
type Device struct {
	ID           string   `json:"id"`
	IEEE         string   `json:"ieee"`    // known to the adapter only
	Model        string   `json:"model"`   // Zigbee2MQTT's model
	Profile      string   `json:"profile"` // ours
	Capabilities []string `json:"capabilities"`
}

// State is the last value of a capability.
type State struct {
	Value string    `json:"value"`
	Since time.Time `json:"since"`
}

// Pending is a command waiting for its confirmation.
type Pending struct {
	Ref       string    `json:"ref"`
	Name      string    `json:"name"`
	DurationS int       `json:"duration_s"`
	Attempt   int       `json:"attempt"`
	Deadline  time.Time `json:"deadline"`
	Exp       time.Time `json:"exp"`
}

// Snapshot is the state the driver saves after every change. When each device last
// spoke is not in it: it changes with every message, and writing it each time would
// wear the SD card. After a restart, the silence of each device counts from then.
type Snapshot struct {
	Devices   map[string]Device           `json:"devices"`
	States    map[string]map[string]State `json:"states"`
	Offline   map[string]bool             `json:"offline"`
	Retired   map[string]bool             `json:"retired"` // ids never given again
	Pending   map[string]Pending          `json:"pending"` // by device: one command at a time
	Bridge    bool                        `json:"bridge"`
	PairUntil time.Time                   `json:"pair_until"`          // zero while closed
	PairRenew time.Time                   `json:"pair_renew,omitzero"` // next request to keep it open, zero if none
	PairRef   string                      `json:"pair_ref"`
	// PairOpen: Zigbee2MQTT confirmed the window (its first piece); until then the
	// network does not count as open.
	PairOpen bool `json:"pair_open,omitzero"`
	// PairAsk is the request to open the network that waits for Zigbee2MQTT's answer.
	PairAsk Asking `json:"pair_ask,omitzero"`
	PairSeq int    `json:"pair_seq,omitzero"` // requests made for the window, to name them
	// Joining are the devices that entered during a pairing window, by IEEE address,
	// while Zigbee2MQTT questions them: they are decided by the window they entered,
	// even if their questioning ends after it (see interviewed).
	Joining map[string]Joining `json:"joining,omitempty"`
	// Heard is the last news of each offline device (see Presence): kept for them only,
	// whose last news no longer moves, so that a message replaying older news is known
	// for what it is after a restart too (see oldNews).
	Heard map[string]time.Time `json:"heard,omitempty"`
}

// Joining is a device that entered during a pairing window, being questioned.
type Joining struct {
	At  time.Time `json:"at"`  // when it entered
	Ref string    `json:"ref"` // the window's pairing order
}

// Asking is a request to open the network waiting for its answer. Without one by
// Deadline it is sent again, Config.Attempts times in all, as a device command is.
type Asking struct {
	Txn      string    `json:"txn"` // the transaction Zigbee2MQTT echoes in its answer
	Attempt  int       `json:"attempt"`
	Deadline time.Time `json:"deadline"`
}

func (s Snapshot) clone() Snapshot {
	s.Devices = maps.Clone(s.Devices)
	states := make(map[string]map[string]State, len(s.States))
	for id, m := range s.States {
		states[id] = maps.Clone(m)
	}
	s.States = states
	s.Offline = maps.Clone(s.Offline)
	s.Retired = maps.Clone(s.Retired)
	s.Pending = maps.Clone(s.Pending)
	s.Joining = maps.Clone(s.Joining)
	s.Heard = maps.Clone(s.Heard)
	return s
}

// Status is the public summary, for zigbee.status.
type Status struct {
	Bridge  bool
	Devices int
	Offline []string // sorted
	Pairing bool
}

// Network is the core.
type Network struct {
	cfg     Config
	newID   func() string
	byName  map[string]Profile
	byModel map[string]Profile
	s       Snapshot
	// seen is when each device's last message reached us, or the start for one silent
	// since: its silence counts from there. news is, for each device that spoke since
	// the start, when Zigbee2MQTT last heard it: its last news (see spokeAt).
	seen map[string]time.Time
	news map[string]time.Time
	// coded: Zigbee2MQTT was told, since it last came online, which devices carry
	// a code. Not saved: it is told again after each start.
	coded bool
	// said is when each device said its last message that carries a last_seen (see
	// noNews). Not saved: the last news of the offline devices is (Snapshot.Heard).
	said map[string]time.Time
	// expelled counts the requests to leave sent to each stranger since the start
	// (see stranger). Not saved: a stranger that speaks after a restart is asked again.
	expelled map[string]int
}

// New creates the core. newID proposes device ids (for example "dev_k7p2"); one
// already given is never reused.
func New(cfg Config, newID func() string) (*Network, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if newID == nil {
		return nil, fmt.Errorf("zigbee: newID is required")
	}
	n := &Network{cfg: cfg, newID: newID, byName: map[string]Profile{}, byModel: map[string]Profile{}}
	for _, p := range cfg.Profiles {
		n.byName[p.Name] = p
		for _, m := range p.Models {
			n.byModel[m] = p
		}
	}
	n.Restore(Snapshot{})
	return n, nil
}

// Snapshot returns a copy of the state.
func (n *Network) Snapshot() Snapshot { return n.s.clone() }

// Restore replaces the state with a saved one.
func (n *Network) Restore(s Snapshot) {
	s = s.clone()
	if s.Devices == nil {
		s.Devices = map[string]Device{}
	}
	if s.States == nil {
		s.States = map[string]map[string]State{}
	}
	if s.Offline == nil {
		s.Offline = map[string]bool{}
	}
	if s.Retired == nil {
		s.Retired = map[string]bool{}
	}
	if s.Pending == nil {
		s.Pending = map[string]Pending{}
	}
	// The adapter closes the network when it stops (no one would vet the devices
	// joining meanwhile): a window still running is opened again at the next input,
	// for what is left of it, or closed for good if it is over.
	if !s.PairUntil.IsZero() {
		s.PairRenew = time.Unix(0, 0)
	}
	// What a device is comes from its profile, which may have changed since it was
	// paired (a reading added, a fix): the saved list is not kept.
	for id, d := range s.Devices {
		if p, ok := n.byName[d.Profile]; ok {
			d.Capabilities = p.capabilities()
			s.Devices[id] = d
		}
	}
	n.s = s
	n.seen, n.news = map[string]time.Time{}, maps.Clone(s.Heard)
	if n.news == nil {
		n.news = map[string]time.Time{}
	}
	n.said, n.coded, n.expelled = map[string]time.Time{}, false, map[string]int{}
}

// Devices lists the registry, sorted by id.
func (n *Network) Devices() []Device {
	out := make([]Device, 0, len(n.s.Devices))
	for _, id := range slices.Sorted(maps.Keys(n.s.Devices)) {
		d := n.s.Devices[id]
		d.Capabilities = slices.Clone(d.Capabilities)
		out = append(out, d)
	}
	return out
}

// Status returns the public summary.
func (n *Network) Status() Status {
	return Status{
		Bridge: n.s.Bridge, Devices: len(n.s.Devices),
		Offline: slices.Sorted(maps.Keys(n.s.Offline)),
		Pairing: (!n.s.PairUntil.IsZero() && n.s.PairOpen) || len(n.s.Joining) > 0,
	}
}

// NextWake returns when the driver must send a Tick, zero if never.
func (n *Network) NextWake() time.Time {
	var next time.Time
	add := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	if !n.s.PairUntil.IsZero() {
		add(n.s.PairUntil)
	}
	if !n.s.PairRenew.IsZero() {
		add(n.s.PairRenew)
	}
	if n.s.PairAsk.Txn != "" {
		add(n.s.PairAsk.Deadline)
	}
	for _, j := range n.s.Joining {
		add(j.At.Add(JoinWait))
	}
	for _, p := range n.s.Pending {
		add(p.Deadline)
	}
	for id, d := range n.s.Devices {
		if n.s.Offline[id] {
			continue
		}
		seen, ok := n.seen[id]
		if !ok {
			add(time.Unix(0, 0)) // its silence starts counting at the next input
			continue
		}
		add(seen.Add(n.offlineAfter(d)))
	}
	return next
}

// Handle processes an input and returns the actions to carry out.
func (n *Network) Handle(in Input) []Action {
	now := in.at()
	for id := range n.s.Devices {
		if _, ok := n.seen[id]; !ok {
			n.seen[id] = now
		}
	}
	var out []Action
	switch v := in.(type) {
	case Report:
		out = n.report(v)
	case Bridge:
		out = n.bridge(v)
	case Interviewed:
		out = n.interviewed(v)
	case Left:
		out = n.left(v)
	case Joined:
		out = n.joined(v)
	case Pair:
		out = n.pair(v)
	case JoinAnswer:
		out = n.joinAnswered(v)
	case Remove:
		out = n.remove(v)
	case Command:
		out = n.command(v)
	}
	return append(out, n.due(now)...)
}

// due runs what the clock has made due: the end of a pairing window or its next
// piece, commands not confirmed in time, devices silent for too long.
func (n *Network) due(now time.Time) []Action {
	out := n.pairingDue(now)
	n.joinsDue(now)
	out = append(out, n.retryPending(now)...)
	return append(out, n.markSilent(now)...)
}

// bridge follows Zigbee2MQTT's state. A pairing window ends when Zigbee2MQTT goes
// offline, and says so to whoever asked for it: Zigbee2MQTT starts again with the
// network closed, and the installer must not wait in front of a closed network. Each
// time it comes online, it is told again which devices carry a code: a keypad paired
// before, or a configuration of Zigbee2MQTT restored from a backup, gets its setting
// back.
func (n *Network) bridge(b Bridge) []Action {
	n.s.Bridge = b.Online
	if !b.Online {
		n.coded = false
		if !n.s.PairUntil.IsZero() {
			return n.failPairing(ReasonBridgeOffline)
		}
		return nil
	}
	if n.coded {
		return nil
	}
	n.coded = true
	var out []Action
	for _, d := range n.Devices() {
		if n.byName[d.Profile].carriesCode() {
			out = append(out, CarriesCode{IEEE: d.IEEE})
		}
	}
	return out
}

// resolve finds a device by its name in Zigbee2MQTT: its id once renamed, its IEEE
// address before.
func (n *Network) resolve(name string) (Device, bool) {
	if d, ok := n.s.Devices[name]; ok {
		return d, true
	}
	return n.byIEEE(name)
}

func (n *Network) byIEEE(ieee string) (Device, bool) {
	for _, d := range n.s.Devices {
		if d.IEEE == ieee {
			return d, true
		}
	}
	return Device{}, false
}
