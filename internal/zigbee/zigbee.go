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

func (n *Network) offlineAfter(d Device) time.Duration {
	if p, ok := n.byName[d.Profile]; ok && p.OfflineAfter > 0 {
		return p.OfflineAfter
	}
	return n.cfg.OfflineAfter
}

// resolve finds a device by its name in Zigbee2MQTT: its id once renamed, its IEEE
// address before.
func (n *Network) resolve(name string) (Device, bool) {
	if d, ok := n.s.Devices[name]; ok {
		return d, true
	}
	for _, d := range n.s.Devices {
		if d.IEEE == name {
			return d, true
		}
	}
	return Device{}, false
}

func (n *Network) byIEEE(ieee string) (Device, bool) {
	for _, d := range n.s.Devices {
		if d.IEEE == ieee {
			return d, true
		}
	}
	return Device{}, false
}

// --- reports -----------------------------------------------------------------

// report reads a device's message. Only changes are announced: Zigbee2MQTT repeats
// the whole state in every message. The first value of a state is recorded without
// an event (nothing changed that anyone saw), except for numbers, which the
// supervision needs to know. Actions are announced every time.
func (n *Network) report(r Report) []Action {
	d, ok := n.resolve(r.Name)
	if !ok {
		return []Action{Ignored{What: "a message from a device that is not registered"}}
	}
	p, ok := n.byName[d.Profile]
	if !ok {
		return []Action{Ignored{What: "a message from a device whose profile is gone: " + d.Profile}}
	}
	var out []Action
	if !r.Retained {
		n.seen[d.ID] = r.Now
		if n.s.Offline[d.ID] {
			delete(n.s.Offline, d.ID)
			out = append(out, Presence{DeviceID: d.ID, Online: true, Since: r.Now})
		}
	}
	for _, c := range p.capabilities() {
		rule := p.Capabilities[c]
		raw, present := r.Fields[rule.From]
		if !present {
			continue
		}
		v, ok := rule.read(raw)
		if !ok {
			continue
		}
		if rule.Momentary {
			if r.Retained {
				continue
			}
			ch := Changed{DeviceID: d.ID, Capability: c, Value: v, Since: r.Now, Notify: true}
			if rule.CodeFrom != "" {
				ch.Code = text(r.Fields[rule.CodeFrom])
			}
			out = append(out, ch)
			continue
		}
		prev, known := n.s.States[d.ID][c]
		if known && !rule.moved(prev.Value, v, n.cfg.Step) {
			continue
		}
		if n.s.States[d.ID] == nil {
			n.s.States[d.ID] = map[string]State{}
		}
		n.s.States[d.ID][c] = State{Value: v, Since: r.Now}
		out = append(out, Changed{
			DeviceID: d.ID, Capability: c, Value: v, Numeric: rule.numeric(), Since: r.Now,
			State: true, Notify: !r.Retained && (known || rule.numeric()),
		})
	}
	if !r.Retained {
		out = append(out, n.confirm(d, p, r)...)
	}
	return out
}

// confirm completes the command waiting on the device if the message reports the
// state it waits for, changed or not.
func (n *Network) confirm(d Device, p Profile, r Report) []Action {
	pend, ok := n.s.Pending[d.ID]
	if !ok {
		return nil
	}
	e := p.Commands[pend.Name].Confirm
	if e == nil {
		return nil
	}
	if _, present := r.Fields[p.Capabilities[e.Capability].From]; !present || n.s.States[d.ID][e.Capability].Value != e.Value {
		return nil
	}
	delete(n.s.Pending, d.ID)
	return []Action{Ack{Ref: pend.Ref, DeviceID: d.ID, Status: AckDone}}
}

func (n *Network) left(l Left) []Action {
	d, ok := n.byIEEE(l.IEEE)
	if !ok || n.s.Offline[d.ID] {
		return nil
	}
	n.s.Offline[d.ID] = true
	return []Action{Presence{DeviceID: d.ID, Since: l.Now}}
}

// --- commands ----------------------------------------------------------------

func (n *Network) command(c Command) []Action {
	d, ok := n.s.Devices[c.DeviceID]
	if !ok {
		return []Action{Ack{Ref: c.Ref, DeviceID: c.DeviceID, Status: AckFailed, Reason: ReasonUnknownDevice}}
	}
	if !c.Exp.IsZero() && !c.Now.Before(c.Exp) {
		return []Action{Ack{Ref: c.Ref, DeviceID: d.ID, Status: AckExpired}}
	}
	spec, ok := n.byName[d.Profile].Commands[c.Name]
	if !ok {
		return []Action{Ack{Ref: c.Ref, DeviceID: d.ID, Status: AckFailed, Reason: ReasonUnsupported}}
	}
	var out []Action
	if old, ok := n.s.Pending[d.ID]; ok {
		delete(n.s.Pending, d.ID)
		out = append(out, Ack{Ref: old.Ref, DeviceID: d.ID, Status: AckFailed, Reason: ReasonSuperseded})
	}
	out = append(out,
		Send{IEEE: d.IEEE, Payload: spec.payload(c.DurationS)},
		Ack{Ref: c.Ref, DeviceID: d.ID, Status: AckAccepted})
	if spec.Confirm == nil {
		return append(out, Ack{Ref: c.Ref, DeviceID: d.ID, Status: AckDone})
	}
	n.s.Pending[d.ID] = Pending{
		Ref: c.Ref, Name: c.Name, DurationS: c.DurationS, Attempt: 1,
		Deadline: c.Now.Add(n.cfg.ConfirmWait), Exp: c.Exp,
	}
	return out
}

// --- pairing and removal -------------------------------------------------------

func (n *Network) pair(p Pair) []Action {
	if p.For <= 0 {
		n.s.PairUntil, n.s.PairRef = time.Time{}, ""
		return []Action{PermitJoin{}}
	}
	d := min(p.For, n.cfg.PairMax)
	n.s.PairUntil, n.s.PairRef = p.Now.Add(d), p.Ref
	return []Action{PermitJoin{Seconds: int(d / time.Second)}}
}

// interviewed decides about a device that joined. Only a model with a profile, during
// a pairing window, is kept: a device we cannot read is a device we cannot watch.
func (n *Network) interviewed(i Interviewed) []Action {
	if _, known := n.byIEEE(i.IEEE); known {
		return []Action{Ignored{What: "a registered device was questioned again"}}
	}
	reject := func(reason, ref string) []Action {
		return []Action{Expel{IEEE: i.IEEE}, Rejected{Ref: ref, Model: i.Model, Reason: reason}}
	}
	switch p, ok := n.byModel[i.Model]; {
	case n.s.PairUntil.IsZero():
		return reject(RejectNotPairing, "")
	case !i.OK:
		return reject(RejectInterviewFailed, n.s.PairRef)
	case !ok:
		return reject(RejectUnknownModel, n.s.PairRef)
	default:
		id := n.freshID()
		if id == "" {
			return append(reject(RejectInterviewFailed, n.s.PairRef), Ignored{What: "no free device id"})
		}
		d := Device{ID: id, IEEE: i.IEEE, Model: i.Model, Profile: p.Name, Capabilities: p.capabilities()}
		n.s.Devices[id] = d
		n.seen[id] = i.Now
		return []Action{Rename{IEEE: i.IEEE, To: id}, Added{Ref: n.s.PairRef, Device: d}, RegistryChanged{Devices: n.Devices()}}
	}
}

func (n *Network) freshID() string {
	for range 100 {
		id := n.newID()
		if id != "" && !n.s.Retired[id] {
			if _, taken := n.s.Devices[id]; !taken {
				return id
			}
		}
	}
	return ""
}

func (n *Network) remove(r Remove) []Action {
	d, ok := n.s.Devices[r.DeviceID]
	if !ok {
		return []Action{Ignored{What: "removal of a device that is not registered"}}
	}
	out := []Action{Expel{IEEE: d.IEEE}}
	if pend, ok := n.s.Pending[d.ID]; ok {
		out = append(out, Ack{Ref: pend.Ref, DeviceID: d.ID, Status: AckFailed, Reason: ReasonRemoved})
	}
	delete(n.s.Devices, d.ID)
	delete(n.s.States, d.ID)
	delete(n.s.Offline, d.ID)
	delete(n.s.Pending, d.ID)
	delete(n.seen, d.ID)
	n.s.Retired[d.ID] = true
	return append(out, Removed{Ref: r.Ref, Device: d}, RegistryChanged{Devices: n.Devices()})
}

// --- timers --------------------------------------------------------------------

// due runs what the clock has made due: the end of a pairing window, commands not
// confirmed in time, devices silent for too long.
func (n *Network) due(now time.Time) []Action {
	var out []Action
	if !n.s.PairUntil.IsZero() && !now.Before(n.s.PairUntil) {
		n.s.PairUntil, n.s.PairRef = time.Time{}, ""
		out = append(out, PermitJoin{})
	}
	for _, id := range slices.Sorted(maps.Keys(n.s.Pending)) {
		p := n.s.Pending[id]
		if now.Before(p.Deadline) {
			continue
		}
		switch {
		case !p.Exp.IsZero() && !now.Before(p.Exp):
			delete(n.s.Pending, id)
			out = append(out, Ack{Ref: p.Ref, DeviceID: id, Status: AckExpired})
		case p.Attempt < n.cfg.Attempts:
			d := n.s.Devices[id]
			p.Attempt++
			p.Deadline = now.Add(n.cfg.ConfirmWait)
			n.s.Pending[id] = p
			out = append(out, Send{IEEE: d.IEEE, Payload: n.byName[d.Profile].Commands[p.Name].payload(p.DurationS)})
		default:
			delete(n.s.Pending, id)
			out = append(out, Ack{Ref: p.Ref, DeviceID: id, Status: AckFailed, Reason: ReasonNoConfirmation})
		}
	}
	for _, id := range slices.Sorted(maps.Keys(n.s.Devices)) {
		if n.s.Offline[id] || now.Before(n.seen[id].Add(n.offlineAfter(n.s.Devices[id]))) {
			continue
		}
		n.s.Offline[id] = true
		out = append(out, Presence{DeviceID: id, Since: now})
	}
	return out
}
