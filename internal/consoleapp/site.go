package consoleapp

import (
	"fmt"
	"maps"
	"slices"

	"safehome/internal/contract"
	"safehome/internal/install"
)

// paired returns the capabilities of a paired device.
func (a *App) paired(id string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.known {
		return nil, ErrNoRegistry
	}
	for _, d := range a.view.Devices {
		if d.DeviceID == id {
			return d.Capabilities, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknown, id)
}

// change reads the site, applies edit, and keeps the result if the hub would take it.
func (a *App) change(edit func(*install.Site) error) error {
	if _, err := a.sessionCtx(); err != nil {
		return err
	}
	a.siteMu.Lock()
	defer a.siteMu.Unlock()
	site, err := a.deps.Sites.Read()
	if err != nil {
		return err
	}
	if site.Zones == nil {
		site.Zones = map[string]install.Zone{}
	}
	if err := edit(&site); err != nil {
		return err
	}
	return a.deps.Sites.Write(site)
}

// siteWithout returns a copy of site where the device id has no zone and is no siren.
func siteWithout(site install.Site, id string) install.Site {
	out := site
	out.Zones = maps.Clone(site.Zones)
	if out.Zones == nil {
		out.Zones = map[string]install.Zone{}
	}
	delete(out.Zones, id)
	out.SirenIDs = slices.DeleteFunc(slices.Clone(site.SirenIDs), func(x string) bool { return x == id })
	return out
}

// SetZone gives a paired device its zone.
func (a *App) SetZone(id, name, kind string) error {
	caps, err := a.paired(id)
	if err != nil {
		return err
	}
	if err := install.ValidName(name); err != nil {
		return err
	}
	if err := install.CanServe(kind, caps); err != nil {
		return err
	}
	err = a.change(func(s *install.Site) error {
		s.Zones[id] = install.Zone{Name: name, Kind: kind}
		return nil
	})
	if err == nil {
		a.log.Info("zone set from the console", "device", id, "kind", kind)
	}
	return err
}

// Unzone takes a device out of its zone.
func (a *App) Unzone(id string) error {
	err := a.change(func(s *install.Site) error {
		if _, ok := s.Zones[id]; !ok {
			return fmt.Errorf("%w: %s", ErrNoZone, id)
		}
		delete(s.Zones, id)
		return nil
	})
	if err == nil {
		a.log.Info("zone removed from the console", "device", id)
	}
	return err
}

// Siren makes a device a siren, or stops. Only a paired device that can sound becomes
// one; any can stop.
func (a *App) Siren(id string, on bool) error {
	if on {
		caps, err := a.paired(id)
		if err != nil {
			return err
		}
		if !slices.Contains(caps, contract.CapSiren) {
			return ErrNotASiren
		}
	}
	err := a.change(func(s *install.Site) error {
		has := slices.Contains(s.SirenIDs, id)
		switch {
		case on && !has:
			s.SirenIDs = append(s.SirenIDs, id)
		case !on && has:
			s.SirenIDs = slices.DeleteFunc(s.SirenIDs, func(x string) bool { return x == id })
			if len(s.SirenIDs) == 0 {
				return ErrLastSiren
			}
		}
		return nil
	})
	if err == nil {
		a.log.Info("siren changed from the console", "device", id, "siren", on)
	}
	return err
}
