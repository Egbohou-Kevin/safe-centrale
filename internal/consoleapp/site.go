package consoleapp

import (
	"fmt"

	"safehome/internal/contract"
	"safehome/internal/install"
)

// paired returns a paired device, as the registry knows it.
func (a *App) paired(id string) (*contract.RegistryDevice, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.known {
		return nil, ErrNoRegistry
	}
	for _, d := range a.view.Devices {
		if d.DeviceID == id {
			return &d, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknown, id)
}

// SetZone gives a paired device its zone.
func (a *App) SetZone(id, name, kind string) error {
	if _, err := a.sessionCtx(); err != nil {
		return err
	}
	d, err := a.paired(id)
	if err != nil {
		return err
	}
	if _, err := a.site.SetZone(id, install.Zone{Name: name, Kind: kind}, d); err != nil {
		return err
	}
	a.log.Info("zone set from the console", "device", id, "kind", kind)
	return nil
}

// Unzone takes a device out of its zone.
func (a *App) Unzone(id string) error {
	if _, err := a.sessionCtx(); err != nil {
		return err
	}
	if _, err := a.site.Unzone(id); err != nil {
		return err
	}
	a.log.Info("zone removed from the console", "device", id)
	return nil
}

// Siren makes a device a siren, or stops. Only a paired device that can sound becomes
// one; any can stop. Asking for what already is does nothing: a form sent twice is no
// mistake.
func (a *App) Siren(id string, on bool) error {
	if _, err := a.sessionCtx(); err != nil {
		return err
	}
	var err error
	if on {
		var d *contract.RegistryDevice
		if d, err = a.paired(id); err != nil {
			return err
		}
		_, err = a.site.AddSiren(id, d)
	} else {
		_, err = a.site.RemoveSiren(id)
	}
	if err != nil {
		return err
	}
	a.log.Info("siren changed from the console", "device", id, "siren", on)
	return nil
}
