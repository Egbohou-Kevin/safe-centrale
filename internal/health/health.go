// Package health supervises the hub and its devices: the hub battery during a
// power cut, sensor batteries, and devices that stopped answering (see
// docs/escalation.md and docs/engine.md). It is a second domain core next to the
// alarm engine, built on the same rules.
//
// It is deterministic: it does not read the clock, write anywhere or talk to any
// hardware. It is given readings that carry their own time and returns alerts for
// a driver to send. It knows nothing of MQTT, JSON or the event contract: the wire
// subpackage translates. An architecture test (internal/archtest) enforces both.
//
// A Monitor is not safe for concurrent use: the driver must feed it one input at
// a time.
package health

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

// Snapshot is the full state of the monitor. The driver writes it to disk after
// every change and hands it back on restart (Restore), so a restart never repeats
// an alert that already went out.
type Snapshot struct {
	MainsLost       bool `json:"mains_lost"`
	HubBatteryKnown bool `json:"hub_battery_known"`
	HubBattery      int  `json:"hub_battery"`

	WarnSent     bool `json:"warn_sent"`     // the first hub battery alert went out
	CriticalSent bool `json:"critical_sent"` // the urgent one went out too
	// Owed is true once a battery alert has gone out: when the mains come back,
	// the contacts get a short message saying so.
	Owed bool `json:"owed"`

	Offline    map[string]bool `json:"offline"`     // devices that stopped answering
	LowBattery map[string]bool `json:"low_battery"` // sensors with a weak battery

	NextSummary time.Time `json:"next_summary"`

	// The components that report their own state and must keep doing so: the GSM
	// gateway (can the SMS backup work?), the Zigbee adapter (can the sensors be
	// heard?) and the link agent (can the cloud be reached?).
	GSM      Watch `json:"gsm"`
	Zigbee   Watch `json:"zigbee"`
	Internet Watch `json:"internet"`
}

func (s Snapshot) clone() Snapshot {
	s.Offline = maps.Clone(s.Offline)
	s.LowBattery = maps.Clone(s.LowBattery)
	return s
}

// Status is the public summary, meant for system.health and the app.
type Status struct {
	MainsLost       bool
	HubBatteryKnown bool
	HubBattery      int
	Offline         []string // sorted
	LowBattery      []string // sorted
	GSM             LinkState
	Zigbee          LinkState
	Internet        LinkState
}

// Monitor is the health monitor.
type Monitor struct {
	cfg   Config
	newID func() string
	s     Snapshot
}

// New creates a monitor. newID produces alert identifiers (for example
// contract.IDGenerator.New).
func New(cfg Config, newID func() string, now time.Time) (*Monitor, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if newID == nil {
		return nil, fmt.Errorf("health: newID is required")
	}
	return &Monitor{cfg: cfg, newID: newID, s: Snapshot{
		Offline: map[string]bool{}, LowBattery: map[string]bool{}, NextSummary: cfg.nextSummary(now),
	}}, nil
}

// Snapshot returns a copy of the current state.
func (m *Monitor) Snapshot() Snapshot { return m.s.clone() }

// Restore replaces the state with a saved one.
func (m *Monitor) Restore(s Snapshot) {
	s = s.clone()
	if s.Offline == nil {
		s.Offline = map[string]bool{}
	}
	if s.LowBattery == nil {
		s.LowBattery = map[string]bool{}
	}
	m.s = s
}

// NextWake returns the time at which the driver must send a Tick: the next daily
// summary, or earlier when a component's silence becomes too long or its alert is
// due.
func (m *Monitor) NextWake() time.Time {
	wake := m.s.NextSummary
	for _, w := range m.watches() {
		if due := w.watch.nextWake(w.rule); !due.IsZero() && due.Before(wake) {
			wake = due
		}
	}
	return wake
}

// Status returns the public summary.
func (m *Monitor) Status() Status {
	return Status{
		MainsLost:       m.s.MainsLost,
		HubBatteryKnown: m.s.HubBatteryKnown,
		HubBattery:      m.s.HubBattery,
		Offline:         slices.Sorted(maps.Keys(m.s.Offline)),
		LowBattery:      slices.Sorted(maps.Keys(m.s.LowBattery)),
		GSM:             m.s.GSM.state(),
		Zigbee:          m.s.Zigbee.state(),
		Internet:        m.s.Internet.state(),
	}
}

// Handle processes an input and returns the alerts to send. A daily summary that
// is due goes first, so it describes the state before this input.
func (m *Monitor) Handle(in Input) []Action {
	out := m.tick(in.at())
	switch v := in.(type) {
	case Mains:
		out = append(out, m.mains(v)...)
	case Battery:
		out = append(out, m.battery(v)...)
	case Availability:
		m.availability(v)
	case Forget:
		delete(m.s.Offline, v.DeviceID)
		delete(m.s.LowBattery, v.DeviceID)
	case GSM:
		m.s.GSM.report(v.Now, v.Seen, v.Usable)
	case Zigbee:
		m.s.Zigbee.report(v.Now, v.Seen, v.Usable)
	case Internet:
		m.s.Internet.report(v.Now, v.Seen, v.Usable)
	}
	for _, w := range m.watches() {
		if w.watch.check(in.at(), w.rule) {
			out = append(out, Alert{Incident: m.newID(), Kind: w.rule.kind, Urgency: UrgencyHigh})
		}
	}
	return out
}

type watched struct {
	watch *Watch
	rule  watchRule
}

// watches lists the supervised components, in a fixed order.
func (m *Monitor) watches() []watched {
	return []watched{
		{&m.s.GSM, watchRule{silentAfter: m.cfg.GSMSilentAfter, alertAfter: m.cfg.GSMDegradedAfter, kind: AlertGSMDegraded}},
		{&m.s.Zigbee, watchRule{silentAfter: m.cfg.ZigbeeSilentAfter, alertAfter: m.cfg.ZigbeeDownAfter, kind: AlertZigbeeDown}},
		// No alert: the contacts cannot act on it, and the cloud sees a silent site by
		// itself. The escalation uses the state to skip the internet.
		{&m.s.Internet, watchRule{silentAfter: m.cfg.InternetSilentAfter}},
	}
}

// --- daily summary ---------------------------------------------------------

// tick sends the daily summary if it is due. After a long suspension it sends one
// summary, not one per missed day.
func (m *Monitor) tick(now time.Time) []Action {
	if now.Before(m.s.NextSummary) {
		return nil
	}
	m.s.NextSummary = m.cfg.nextSummary(now)

	var out []Action
	if len(m.s.Offline) > 0 {
		out = append(out, Alert{
			Incident: m.newID(), Kind: AlertDeviceOffline, Urgency: UrgencyLow,
			Devices: slices.Sorted(maps.Keys(m.s.Offline)),
		})
	}
	if len(m.s.LowBattery) > 0 {
		out = append(out, Alert{
			Incident: m.newID(), Kind: AlertBatteryLow, Urgency: UrgencyLow,
			Devices: slices.Sorted(maps.Keys(m.s.LowBattery)),
		})
	}
	return out
}

// --- sensors ---------------------------------------------------------------

func (m *Monitor) availability(a Availability) {
	if a.DeviceID == m.cfg.HubDeviceID {
		return // the hub reports its own faults another way
	}
	if a.Online {
		delete(m.s.Offline, a.DeviceID)
	} else {
		m.s.Offline[a.DeviceID] = true
	}
}

func (m *Monitor) battery(b Battery) []Action {
	if b.Percent < 0 || b.Percent > 100 {
		return nil // not a reading
	}
	if b.DeviceID == m.cfg.HubDeviceID {
		return m.hubBattery(b.Percent)
	}
	switch {
	case b.Percent <= m.cfg.DeviceBatteryLow:
		m.s.LowBattery[b.DeviceID] = true
	case b.Percent > m.cfg.DeviceBatteryRearm:
		delete(m.s.LowBattery, b.DeviceID)
	}
	return nil
}

// --- hub: mains and battery -------------------------------------------------

// mains records the supply. A power cut alone is not an alert: cuts are frequent
// and the hub runs on its battery. The return of the power is announced only if a
// battery alert went out.
func (m *Monitor) mains(v Mains) []Action {
	was := m.s.MainsLost
	m.s.MainsLost = v.Lost
	if v.Lost {
		return m.checkHubBattery()
	}
	if !was || !m.s.Owed {
		return nil
	}
	m.s.Owed = false
	a := Alert{Incident: m.newID(), Kind: AlertMainsRestored, Urgency: UrgencyLow}
	if m.s.HubBatteryKnown {
		a.Percent = m.s.HubBattery
	}
	return []Action{a}
}

func (m *Monitor) hubBattery(percent int) []Action {
	m.s.HubBatteryKnown, m.s.HubBattery = true, percent
	if percent > m.cfg.HubBatteryRearm {
		m.s.WarnSent, m.s.CriticalSent = false, false
	}
	return m.checkHubBattery()
}

// checkHubBattery raises at most one alert, for the lowest threshold crossed, and
// only while the hub runs on its battery.
func (m *Monitor) checkHubBattery() []Action {
	if !m.s.MainsLost || !m.s.HubBatteryKnown {
		return nil
	}
	p := m.s.HubBattery
	var urgency Urgency
	switch {
	case p <= m.cfg.HubBatteryCritical && !m.s.CriticalSent:
		urgency = UrgencyCritical
	case p <= m.cfg.HubBatteryWarn && p > m.cfg.HubBatteryCritical && !m.s.WarnSent:
		urgency = UrgencyHigh
	default:
		return nil
	}
	if urgency == UrgencyCritical {
		m.s.CriticalSent = true
	}
	m.s.WarnSent = true // a jump straight to the urgent level also uses up the first one
	m.s.Owed = true
	return []Action{Alert{
		Incident: m.newID(), Kind: AlertHubBatteryLow, Urgency: urgency, Percent: p,
	}}
}
