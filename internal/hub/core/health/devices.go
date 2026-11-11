package health

import (
	"maps"
	"slices"
	"time"
)

// The device list: what the monitor knows of each registered device, for the owner's
// list of sensors (system.devices, see docs/contract.md). Nothing in it moves with the
// shop's activity: a device that answers is only said to answer, and an offline one
// keeps the times it went offline with. Door and motion states never reach the
// monitor.

// Zone is the zone of a device in the site's configuration: its name ("Entrance"),
// its kind ("entry", "instant", "24h_fire"...) and its room ("Salon", empty when the
// installer gave none), as the configuration writes them.
type Zone struct {
	Name string
	Kind string
	Room string
}

// Absence is when an offline device went offline, and when it was last heard before
// (zero when it said nothing since the adapter's restart). Both stay fixed while it
// is offline.
type Absence struct {
	Since time.Time `json:"since,omitzero"`
	Heard time.Time `json:"heard,omitzero"`
}

// Device is one device of the list.
type Device struct {
	ID           string
	Capabilities []string // as the registry gives them
	Zone         Zone     // zero for a device outside any zone
	Online       bool
	// Only while offline, each zero when unknown: since when, and its last news (see
	// Absence).
	Since, Heard time.Time
	// The last battery reading in percent, rounded to the nearest ten, when one was
	// read; BatteryLow follows the sensor batteries' rule (low at DeviceBatteryLow,
	// fine again above DeviceBatteryRearm).
	Battery      int
	BatteryKnown bool
	BatteryLow   bool
}

// Devices returns the list, sorted by id: the registered devices only, whatever the
// monitor heard of others. A device not known to be offline answers: the adapter
// says when one stops.
func (m *Monitor) Devices() []Device {
	out := make([]Device, 0, len(m.s.Registry))
	for _, id := range slices.Sorted(maps.Keys(m.s.Registry)) {
		d := Device{
			ID: id, Capabilities: slices.Clone(m.s.Registry[id]), Zone: m.cfg.Zones[id],
			Online: !m.s.Offline[id], BatteryLow: m.s.LowBattery[id],
		}
		if !d.Online {
			a := m.s.Absences[id]
			d.Since, d.Heard = a.Since, a.Heard
		}
		d.Battery, d.BatteryKnown = m.s.Batteries[id]
		out = append(out, d)
	}
	return out
}

// registry takes the adapter's new list of devices.
func (m *Monitor) registry(r Registry) {
	reg := make(map[string][]string, len(r.Devices))
	for id, caps := range r.Devices {
		reg[id] = slices.Clone(caps)
	}
	m.s.Registry = reg
}

// forget drops a device that left the registry, with all the monitor knew of it.
func (m *Monitor) forget(id string) {
	delete(m.s.Offline, id)
	delete(m.s.LowBattery, id)
	delete(m.s.Registry, id)
	delete(m.s.Batteries, id)
	delete(m.s.Absences, id)
}

// roundTen keeps a percentage to the nearest ten: the list need not move with every
// point a battery loses.
func roundTen(percent int) int { return (min(max(percent, 0), 100) + 5) / 10 * 10 }
