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
	PairUntil time.Time                   `json:"pair_until"` // zero while closed
	PairRef   string                      `json:"pair_ref"`
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
	seen    map[string]time.Time // when each device last spoke
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
	// What a device is comes from its profile, which may have changed since it was
	// paired (a reading added, a fix): the saved list is not kept.
	for id, d := range s.Devices {
		if p, ok := n.byName[d.Profile]; ok {
			d.Capabilities = p.capabilities()
			s.Devices[id] = d
		}
	}
	n.s = s
	n.seen = map[string]time.Time{}
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
		Offline: slices.Sorted(maps.Keys(n.s.Offline)), Pairing: !n.s.PairUntil.IsZero(),
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
		n.s.Bridge = v.Online
	case Interviewed:
		out = n.interviewed(v)
	case Left:
		out = n.left(v)
	case Pair:
		out = n.pair(v)
	case Remove:
		out = n.remove(v)
	case Command:
		out = n.command(v)
	}
	return append(out, n.due(now)...)
}

// due runs what the clock has made due: the end of a pairing window, commands not
// confirmed in time, devices silent for too long.
func (n *Network) due(now time.Time) []Action {
	var out []Action
	if !n.s.PairUntil.IsZero() && !now.Before(n.s.PairUntil) {
		n.s.PairUntil, n.s.PairRef = time.Time{}, ""
		out = append(out, PermitJoin{})
	}
	out = append(out, n.retryPending(now)...)
	return append(out, n.markSilent(now)...)
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
