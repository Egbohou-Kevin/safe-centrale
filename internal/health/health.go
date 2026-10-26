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

	// For the device list (Devices): the registered devices with their capabilities,
	// the last battery reading of each sensor rounded to ten (all the list says of it),
	// and since when and since what news each offline device is offline.
	Registry  map[string][]string `json:"registry"`
	Batteries map[string]int      `json:"batteries"`
	Absences  map[string]Absence  `json:"absences"`

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
	s.Registry = maps.Clone(s.Registry) // a device's list is replaced, never changed in place
	s.Batteries = maps.Clone(s.Batteries)
	s.Absences = maps.Clone(s.Absences)
	return s
}

// fill makes the maps a saved state may lack (an empty one, or one saved before a
// map existed).
func (s *Snapshot) fill() {
	s.Offline, s.LowBattery = orEmpty(s.Offline), orEmpty(s.LowBattery)
	s.Registry, s.Batteries, s.Absences = orEmpty(s.Registry), orEmpty(s.Batteries), orEmpty(s.Absences)
}

func orEmpty[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return map[K]V{}
	}
	return m
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
	s := Snapshot{NextSummary: cfg.nextSummary(now)}
	s.fill()
	return &Monitor{cfg: cfg, newID: newID, s: s}, nil
}

// Snapshot returns a copy of the current state.
func (m *Monitor) Snapshot() Snapshot { return m.s.clone() }

// Restore replaces the state with a saved one. What is not saved (when each
// component last spoke) is dropped, as a load from disk would. A summary time later
// than the monitor's own (computed by New from the clock at start) was saved while
// the clock ran ahead: the summary would keep quiet about the faults until then,
// restarts included. The monitor's own stands instead.
func (m *Monitor) Restore(s Snapshot) {
	s = s.clone()
	s.fill()
	if !m.s.NextSummary.IsZero() && s.NextSummary.After(m.s.NextSummary) {
		s.NextSummary = m.s.NextSummary
	}
	m.s = s
	for _, w := range m.watches() {
		w.watch.forget()
	}
}

// Stepped takes a forward step d of the system clock, which the driver sees on the
// monotonic clock (driver.Core.Stepped): a Pi without a battery clock starts from the
// last time it knew, hours late after a power cut, and NTP sets it forward once
// internet is there. The silence of each component it watches (the GSM gateway, the
// Zigbee adapter, the link agent) keeps the time that really passed: a gateway heard a
// minute ago is not silent for hours, and no zigbee_down or gsm_degraded goes out for
// it. The daily summary is a time of day, not a wait: it stays, and so do the times of
// the device list, which are only said.
func (m *Monitor) Stepped(d time.Duration) {
	if d <= 0 {
		return
	}
	for _, w := range m.watches() {
		w.watch.stepped(d)
	}
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
		GSM:             m.gsmState(),
		Zigbee:          m.s.Zigbee.state(),
		Internet:        m.s.Internet.state(),
	}
}

// Handle processes an input and returns the alerts to send. A daily summary that
// is due goes first, so it describes the state before this input.
func (m *Monitor) Handle(in Input) []Action {
	for _, w := range m.watches() {
		w.watch.resume(in.at(), w.rule)
	}
	out := m.tick(in.at())
	switch v := in.(type) {
	case Mains:
		out = append(out, m.mains(v)...)
	case Battery:
		out = append(out, m.battery(v)...)
	case Availability:
		m.availability(v)
	case Registry:
		m.registry(v)
	case Forget:
		m.forget(v.DeviceID)
	case GSM:
		if !m.cfg.WithoutGSM { // a gateway run against the configuration says nothing
			m.s.GSM.report(v.Now, v.Seen, v.Usable)
		}
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

// watches lists the supervised components, in a fixed order. A site without a GSM
// line watches no gateway, even one it heard before its configuration changed.
func (m *Monitor) watches() []watched {
	out := []watched{
		{&m.s.Zigbee, watchRule{silentAfter: m.cfg.ZigbeeSilentAfter, alertAfter: m.cfg.ZigbeeDownAfter, kind: AlertZigbeeDown}},
		// No alert: the contacts cannot act on it, and the cloud sees a silent site by
		// itself. The escalation uses the state to skip the internet.
		{&m.s.Internet, watchRule{silentAfter: m.cfg.InternetSilentAfter}},
	}
	if m.cfg.WithoutGSM {
		return out
	}
	gsm := watched{&m.s.GSM, watchRule{silentAfter: m.cfg.GSMSilentAfter, alertAfter: m.cfg.GSMDegradedAfter, kind: AlertGSMDegraded}}
	return append([]watched{gsm}, out...)
}

// gsmState is the gsm link: none on a site without a GSM line, else what the
// gateway's watch knows.
func (m *Monitor) gsmState() LinkState {
	if m.cfg.WithoutGSM {
		return LinkNone
	}
	return m.s.GSM.state()
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

// availability records whether a device answers, and for one that does not, the
// times the adapter gives: the latest word says it all.
func (m *Monitor) availability(a Availability) {
	id := a.DeviceID
	if id == m.cfg.HubDeviceID {
		return // the hub reports its own faults another way
	}
	if a.Online {
		delete(m.s.Offline, id)
		delete(m.s.Absences, id)
		return
	}
	m.s.Offline[id] = true
	if a.Since.IsZero() && a.Heard.IsZero() {
		delete(m.s.Absences, id)
		return
	}
	m.s.Absences[id] = Absence{Since: a.Since, Heard: a.Heard}
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
	m.s.Batteries[b.DeviceID] = roundTen(b.Percent)
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
		if !was {
			// A new cut: the urgent alert is due again, the hub may go dark during
			// it. The first one goes once per recharge (see hubBattery).
			m.s.CriticalSent = false
		}
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
