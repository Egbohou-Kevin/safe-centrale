// Package power is the core of the hub's battery module (see docs/alimentation.md):
// it turns the readings of the mains line and of the fuel gauge into what the site is
// told, the mains and the battery of the hub_device_id device, and decides when to
// shut the hub down before its cell is empty. The supervision (internal/health) then
// raises battery_low_hub and mains_restored from those readings.
//
// Like the other domain cores, this package does no I/O, never reads the clock and
// knows nothing of the contract: inputs carry their time, internal/power/wire
// translates.
package power

import (
	"errors"
	"time"
)

// Config holds the core's settings.
type Config struct {
	// MainsSettle: a change of the mains line counts once it has held this long, so
	// that a flicker of the supply is not a power cut.
	MainsSettle time.Duration
	// BatteryStep is the change of charge, in points, worth telling the site.
	BatteryStep int
	// CutMilliVolts and CutAfter: on battery, a cell below CutMilliVolts for CutAfter
	// is nearly empty, and the hub asks the module to shut it down cleanly.
	CutMilliVolts int
	CutAfter      time.Duration
}

// DefaultConfig returns the defaults of docs/alimentation.md: 3.0 V is where the
// module maker's own script shuts the Pi down.
func DefaultConfig() Config {
	return Config{MainsSettle: 3 * time.Second, BatteryStep: 2, CutMilliVolts: 3000, CutAfter: 30 * time.Second}
}

// Validate checks the settings.
func (c Config) Validate() error {
	switch {
	case c.MainsSettle < 0 || c.MainsSettle > time.Minute:
		return errors.New("power: MainsSettle from 0 to 1 min")
	case c.BatteryStep < 1 || c.BatteryStep > 20:
		return errors.New("power: BatteryStep from 1 to 20 points")
	case c.CutMilliVolts < 2800 || c.CutMilliVolts > 3600:
		return errors.New("power: CutMilliVolts from 2800 to 3600 (a lithium cell)")
	case c.CutAfter < time.Second || c.CutAfter > 10*time.Minute:
		return errors.New("power: CutAfter from 1 s to 10 min")
	}
	return nil
}

// Snapshot is the state saved between runs: none. The readings are told again at
// each start, and the module is read again.
type Snapshot struct{}

// Monitor is the core.
type Monitor struct {
	cfg Config

	mainsKnown bool
	lost       bool // the supply as told to the site
	settling   bool // the line reads the other way since settleFrom
	settleFrom time.Time

	batteryKnown bool
	percent      int // the charge as told to the site

	lowFrom  time.Time // on battery, the cell is below CutMilliVolts since then
	cutAsked bool
}

// New builds the core.
func New(cfg Config) (*Monitor, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Monitor{cfg: cfg}, nil
}

// Snapshot returns the state to save.
func (m *Monitor) Snapshot() Snapshot { return Snapshot{} }

// Restore puts a saved state back.
func (m *Monitor) Restore(Snapshot) {}

// NextWake returns when the core wants a tick: never, it moves with the readings.
func (m *Monitor) NextWake() time.Time { return time.Time{} }

// Stepped takes a forward step d of the system clock, which the driver sees on the
// monotonic clock (driver.Core.Stepped): a Pi without a battery clock starts from the
// last time it knew, hours late after a power cut, and NTP sets it forward once
// internet is there, which the 4G gives during a cut too. The waits under way move with
// the clock, so that each keeps the time that really passed: a cell low for a few
// seconds does not cut the hub at once, and a line read the other way once does not
// change the supply.
func (m *Monitor) Stepped(d time.Duration) {
	if d <= 0 {
		return
	}
	if !m.settleFrom.IsZero() {
		m.settleFrom = m.settleFrom.Add(d)
	}
	if !m.lowFrom.IsZero() {
		m.lowFrom = m.lowFrom.Add(d)
	}
}

// Handle processes one input.
func (m *Monitor) Handle(in Input) []Action {
	switch v := in.(type) {
	case Line:
		return m.line(v)
	case Gauge:
		return m.gauge(v)
	}
	return nil
}

func (m *Monitor) line(v Line) []Action {
	if !m.mainsKnown {
		m.mainsKnown, m.lost = true, v.Lost
		return []Action{Mains{Lost: v.Lost}}
	}
	if v.Lost == m.lost {
		m.settling = false
		return nil
	}
	if !m.settling {
		m.settling, m.settleFrom = true, v.Now
	}
	if v.Now.Sub(m.settleFrom) < m.cfg.MainsSettle {
		return nil
	}
	m.lost, m.settling = v.Lost, false
	if !m.lost {
		m.lowFrom, m.cutAsked = time.Time{}, false
	}
	return []Action{Mains{Lost: m.lost}}
}

func (m *Monitor) gauge(v Gauge) []Action {
	if v.MilliVolts <= 0 {
		return nil // not a reading
	}
	var out []Action
	p := min(max(v.Percent, 0), 100)
	if !m.batteryKnown || abs(p-m.percent) >= m.cfg.BatteryStep || (p != m.percent && (p == 0 || p == 100)) {
		m.batteryKnown, m.percent = true, p
		out = append(out, Battery{Percent: p})
	}
	if cut, ok := m.nearlyEmpty(v); ok {
		out = append(out, cut)
	}
	return out
}

// nearlyEmpty asks for the cut once the cell has stayed low on battery for CutAfter.
func (m *Monitor) nearlyEmpty(v Gauge) (Cut, bool) {
	if !m.mainsKnown || !m.lost || v.MilliVolts >= m.cfg.CutMilliVolts {
		m.lowFrom = time.Time{}
		return Cut{}, false
	}
	if m.lowFrom.IsZero() {
		m.lowFrom = v.Now
	}
	if m.cutAsked || v.Now.Sub(m.lowFrom) < m.cfg.CutAfter {
		return Cut{}, false
	}
	m.cutAsked = true
	return Cut{MilliVolts: v.MilliVolts}, true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
