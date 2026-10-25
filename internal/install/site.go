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

// Site is the use of the devices: their zones and the sirens. SirenNames names some
// sirens for people, by id (see SirenName).
type Site struct {
	Zones      map[string]Zone
	SirenIDs   []string
	SirenNames map[string]string
}

// Without returns a copy of the site where the device id has no zone and is no siren.
func (s Site) Without(id string) Site {
	out := Site{Zones: maps.Clone(s.Zones), SirenIDs: slices.DeleteFunc(slices.Clone(s.SirenIDs), func(x string) bool { return x == id }),
		SirenNames: unnamed(s.SirenNames, id)}
	if out.Zones == nil {
		out.Zones = map[string]Zone{}
	}
	delete(out.Zones, id)
	return out
}

// defaultSirenName is what the texts call a siren without a name ("Sirene").
const defaultSirenName = "Sir\u00e8ne"

// SirenName is what the alerts call the siren id. A device with a zone keeps its
// zone's name. Another has the name it was given, or by default "Sirene" when it is
// the site's only siren without a zone, and "Sirene 1", "Sirene 2"... (in the order
// they were added) when there are several: a siren with a zone is not counted, so no
// "Sirene 2" goes without a "Sirene 1".
func (s Site) SirenName(id string) string {
	if z, zoned := s.Zones[id]; zoned {
		return z.Name
	}
	if n := s.SirenNames[id]; n != "" {
		return n
	}
	plain := s.plainSirens()
	rank := slices.Index(plain, id)
	if len(plain) < 2 || rank < 0 {
		return defaultSirenName
	}
	return fmt.Sprintf("%s %d", defaultSirenName, rank+1)
}

// plainSirens lists the sirens without a zone, in the order they were added: the
// ones a name, or a default name, is for.
func (s Site) plainSirens() []string {
	return slices.DeleteFunc(slices.Clone(s.SirenIDs), func(id string) bool {
		_, zoned := s.Zones[id]
		return zoned
	})
}

// CheckSirenName checks a name for the siren id before it is given: the zones' rule
// (ValidSirenName), and no zone, since the alerts call a device with a zone by its
// zone's name (ErrHasZone).
func (s Site) CheckSirenName(id, name string) error {
	if err := ValidSirenName(name); err != nil {
		return err
	}
	if _, zoned := s.Zones[id]; zoned {
		return fmt.Errorf("%s %w", id, ErrHasZone)
	}
	return nil
}

// unnamed returns a copy of names without the siren id.
func unnamed(names map[string]string, id string) map[string]string {
	out := maps.Clone(names)
	delete(out, id)
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
	ErrNoSiren     = errors.New("is not a siren")
	ErrHasZone     = errors.New("has a zone: the alerts call it by its zone's name")
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
// not know (one of another adapter) unchecked. A siren's name gives way to the zone's:
// the alerts call a device with a zone by its zone's name (SirenName).
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
		s.SirenNames = unnamed(s.SirenNames, id)
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

// NameSiren gives the siren id its name for people, and returns the name it had
// ("" for none, the default of SirenName). The name follows the zones' rule
// (ValidSirenName): it goes into the same texts. A siren with a zone has its zone's
// name (CheckSirenName).
func (e *Editor) NameSiren(id, name string) (old string, err error) {
	if err := ValidSirenName(name); err != nil {
		return "", err
	}
	err = e.edit(func(s *Site) (bool, error) {
		if !slices.Contains(s.SirenIDs, id) {
			return false, fmt.Errorf("%s %w", id, ErrNoSiren)
		}
		if err := s.CheckSirenName(id, name); err != nil {
			return false, err
		}
		if old = s.SirenNames[id]; old == name {
			return false, nil
		}
		s.SirenNames = maps.Clone(s.SirenNames)
		if s.SirenNames == nil {
			s.SirenNames = map[string]string{}
		}
		s.SirenNames[id] = name
		return true, nil
	})
	return old, err
}

// RemoveSiren stops the device id being a siren, and forgets its name; removed is
// false when it was none. The last siren stays (ErrLastSiren).
func (e *Editor) RemoveSiren(id string) (removed bool, err error) {
	err = e.edit(func(s *Site) (bool, error) {
		if !slices.Contains(s.SirenIDs, id) {
			return false, nil
		}
		if len(s.SirenIDs) == 1 {
			return false, ErrLastSiren
		}
		s.SirenIDs = slices.DeleteFunc(s.SirenIDs, func(x string) bool { return x == id })
		s.SirenNames = unnamed(s.SirenNames, id)
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
