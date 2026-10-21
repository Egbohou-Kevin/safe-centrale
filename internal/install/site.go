package install

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"safehome/internal/contract"
)

// Zone is what a device is for: a name for people and a kind for the engine.
type Zone struct {
	Name string
	Kind string
}

// Site is the use of the devices: their zones and the sirens.
type Site struct {
	Zones    map[string]Zone
	SirenIDs []string
}

// Without returns a copy of the site where the device id has no zone and is no siren.
func (s Site) Without(id string) Site {
	out := Site{Zones: maps.Clone(s.Zones), SirenIDs: slices.DeleteFunc(slices.Clone(s.SirenIDs), func(x string) bool { return x == id })}
	if out.Zones == nil {
		out.Zones = map[string]Zone{}
	}
	delete(out.Zones, id)
	return out
}

// Sites keeps the site: the hub's files, which the program reads and writes. Check
// and Write refuse what the hub would refuse.
type Sites interface {
	Read() (Site, error)
	Check(Site) error
	Write(Site) error
}

// The refusals of a change of the site, for a tool to explain them.
var (
	ErrNoZone      = errors.New("has no zone")
	ErrCannotSound = errors.New("cannot sound")
	// ErrLastSiren: the hub refuses a site without a siren. The editor says so before
	// asking the hub, for a tool to explain it.
	ErrLastSiren = errors.New("the site must keep at least one siren")
)

// Editor changes the use of the devices in the site that Sites keeps, for the
// command-line tool and the local console alike. The changes wait for each other:
// each one reads the site, edits it and writes it, and two at once would lose one.
type Editor struct {
	sites Sites
	mu    sync.Mutex
}

// NewEditor returns an editor of the site that sites keeps.
func NewEditor(sites Sites) *Editor { return &Editor{sites: sites} }

// Read returns the site.
func (e *Editor) Read() (Site, error) { return e.sites.Read() }

// edit reads the site, applies change, and writes the result when change says so.
func (e *Editor) edit(change func(*Site) (write bool, err error)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	site, err := e.sites.Read()
	if err != nil {
		return err
	}
	if site.Zones == nil {
		site.Zones = map[string]Zone{}
	}
	write, err := change(&site)
	if err != nil || !write {
		return err
	}
	return e.sites.Write(site)
}

// ValidZone checks a zone's name and kind, whatever the device.
func ValidZone(z Zone) error {
	if err := ValidName(z.Name); err != nil {
		return err
	}
	return ValidKind(z.Kind)
}

// SetZone gives the device id the zone z, and returns the zone it had, if any. d is
// the device as the Zigbee registry knows it; nil accepts a device the registry does
// not know (one of another adapter) unchecked.
func (e *Editor) SetZone(id string, z Zone, d *contract.RegistryDevice) (old *Zone, err error) {
	if err := ValidZone(z); err != nil {
		return nil, err
	}
	if d != nil {
		if err := CanServe(z.Kind, d.Capabilities); err != nil {
			return nil, fmt.Errorf("%s cannot serve as this zone: %w", id, err)
		}
	}
	err = e.edit(func(s *Site) (bool, error) {
		if had, ok := s.Zones[id]; ok {
			old = &had
		}
		s.Zones[id] = z
		return true, nil
	})
	return old, err
}

// Unzone takes the device id out of its zone, and returns that zone.
func (e *Editor) Unzone(id string) (Zone, error) {
	var z Zone
	err := e.edit(func(s *Site) (bool, error) {
		var ok bool
		if z, ok = s.Zones[id]; !ok {
			return false, fmt.Errorf("%s %w", id, ErrNoZone)
		}
		delete(s.Zones, id)
		return true, nil
	})
	return z, err
}

// AddSiren makes the device id a siren; added is false when it was one already. d is
// as for SetZone.
func (e *Editor) AddSiren(id string, d *contract.RegistryDevice) (added bool, err error) {
	if d != nil && !slices.Contains(d.Capabilities, contract.CapSiren) {
		return false, fmt.Errorf("%s %w (it has %s)", id, ErrCannotSound, strings.Join(d.Capabilities, ", "))
	}
	err = e.edit(func(s *Site) (bool, error) {
		if slices.Contains(s.SirenIDs, id) {
			return false, nil
		}
		s.SirenIDs = append(s.SirenIDs, id)
		added = true
		return true, nil
	})
	return added, err
}

// RemoveSiren stops the device id being a siren; removed is false when it was none.
// The last siren stays (ErrLastSiren).
func (e *Editor) RemoveSiren(id string) (removed bool, err error) {
	err = e.edit(func(s *Site) (bool, error) {
		if !slices.Contains(s.SirenIDs, id) {
			return false, nil
		}
		if len(s.SirenIDs) == 1 {
			return false, ErrLastSiren
		}
		s.SirenIDs = slices.DeleteFunc(s.SirenIDs, func(x string) bool { return x == id })
		removed = true
		return true, nil
	})
	return removed, err
}

// Use is what a device does in the site.
type Use struct {
	Zone  *Zone // nil: no zone
	Siren bool
}

// Used says whether the device does anything.
func (u Use) Used() bool { return u.Zone != nil || u.Siren }

func useOf(s Site, id string) Use {
	u := Use{Siren: slices.Contains(s.SirenIDs, id)}
	if z, ok := s.Zones[id]; ok {
		u.Zone = &z
	}
	return u
}

// CheckRemoval says whether the site can do without the device id, before it is
// removed from the network: a removal cannot be undone (the device would have to be
// paired again, with a new id). It returns what the device does now.
func (e *Editor) CheckRemoval(id string) (Use, error) {
	site, err := e.sites.Read()
	if err != nil {
		return Use{}, err
	}
	u := useOf(site, id)
	if !u.Used() {
		return u, nil
	}
	if without := site.Without(id); u.Siren && len(without.SirenIDs) == 0 {
		err = ErrLastSiren
	} else {
		err = e.sites.Check(without)
	}
	if err != nil {
		return u, fmt.Errorf("%s cannot be removed yet, the configuration could not follow (add its replacement first): %w", id, err)
	}
	return u, nil
}

// FinishRemoval takes the device id out of the site once it is out of the network,
// and returns what it did there.
func (e *Editor) FinishRemoval(id string) (Use, error) {
	var u Use
	err := e.edit(func(s *Site) (bool, error) {
		if u = useOf(*s, id); !u.Used() {
			return false, nil
		}
		*s = s.Without(id)
		return true, nil
	})
	if err != nil {
		return u, fmt.Errorf("the device is removed but the configuration still uses it: %w", err)
	}
	return u, nil
}
