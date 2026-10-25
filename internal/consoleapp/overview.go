package consoleapp

import (
	"maps"
	"slices"
	"strings"
	"time"

	"safehome/internal/contract"
	"safehome/internal/install"
)

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
	// Leftovers are the users of codes who are no longer contacts: to remove.
	Leftovers []string
	CodesErr  string
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
	// SirenName is what the alerts call it, while a siren: its zone's name when it
	// has a zone, or else the name it was given (SirenNamed), or the default.
	SirenName  string
	SirenNamed bool
	CanSound   bool
	Keypad     bool
	Kinds      []string // the kinds of zone it can serve
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
	site, siteErr := a.site.Read()
	people, leftovers, codesErr := a.deps.Codes.People()
	o, registered, events := a.snapshot()

	o.People, o.Leftovers = people, leftovers
	if codesErr != nil {
		o.CodesErr = codesErr.Error()
	}
	if siteErr != nil {
		o.SiteErr = siteErr.Error()
		site = install.Site{Zones: map[string]install.Zone{}}
	}
	o.Devices = devices(site, registered, o.Status)
	if o.Known && siteErr == nil {
		o.Findings = install.Check(site, registered)
	}
	o.Events = latestFirst(events, site)
	return o
}

// snapshot copies the console's state: the overview's own fields, the registered
// devices and the events of the mode.
func (a *App) snapshot() (Overview, []contract.RegistryDevice, []Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	o := Overview{Open: a.open, Until: a.until, Hotspot: a.hotspotErr, Known: a.known, Status: a.view.Status, Busy: a.busy}
	if a.pairing != nil {
		p := *a.pairing
		p.Added, p.Rejected = slices.Clone(p.Added), slices.Clone(p.Rejected)
		o.Pairing = &p
	}
	return o, slices.Clone(a.view.Devices), slices.Clone(a.events)
}

// devices lists the registered devices, then what the site uses without it being
// registered (a mistake to show, and to undo), sorted by id.
func devices(site install.Site, registered []contract.RegistryDevice, status *contract.ZigbeeStatusData) []Device {
	var offline []string
	if status != nil {
		offline = status.Offline
	}
	var out []Device
	seen := map[string]bool{}
	for _, d := range registered {
		seen[d.DeviceID] = true
		out = append(out, newDevice(site, d.DeviceID, d.Model, d.Capabilities, true, slices.Contains(offline, d.DeviceID)))
	}
	used := slices.Concat(slices.Collect(maps.Keys(site.Zones)), site.SirenIDs)
	for _, id := range used {
		if !seen[id] {
			seen[id] = true
			out = append(out, newDevice(site, id, "", nil, false, false))
		}
	}
	slices.SortFunc(out, func(x, y Device) int { return strings.Compare(x.ID, y.ID) })
	return out
}

func newDevice(site install.Site, id, model string, caps []string, paired, offline bool) Device {
	d := Device{ID: id, Model: model, Capabilities: caps, Paired: paired, Offline: offline,
		Siren: slices.Contains(site.SirenIDs, id), CanSound: slices.Contains(caps, contract.CapSiren),
		Keypad: slices.Contains(caps, contract.CapKeypad)}
	if z, ok := site.Zones[id]; ok {
		d.Zone = &z
	}
	if d.Siren {
		d.SirenName = site.SirenName(id)
		_, d.SirenNamed = site.SirenNames[id]
	}
	for _, k := range install.ZoneKinds() {
		if install.CanServe(k, caps) == nil {
			d.Kinds = append(d.Kinds, k)
		}
	}
	return d
}

// latestFirst returns the events from the latest, each with its zone's name.
func latestFirst(events []Event, site install.Site) []Event {
	out := make([]Event, 0, len(events))
	for _, e := range slices.Backward(events) {
		if z, ok := site.Zones[e.DeviceID]; ok {
			e.Zone = z.Name
		}
		out = append(out, e)
	}
	return out
}
