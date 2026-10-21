package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"safehome/internal/access"
	"safehome/internal/driver"
	"safehome/internal/escalation"
)

// OpenCodes opens the access codes saved in store, for the installer's tools (the
// command-line tool, the local console), which set them without running a hub. Every
// change is saved before it takes effect; the running hub reads them again when told
// (ReloadCodes).
func OpenCodes(cfg access.Config, store driver.Store) (*access.Registry, error) {
	return openRegistry(cfg, store, keyAccess)
}

func openRegistry(cfg access.Config, store driver.Store, key string) (*access.Registry, error) {
	records, err := loadCodes(store, key)
	if err != nil {
		return nil, err
	}
	persist := func(rs []access.Record) error {
		b, err := json.Marshal(rs)
		if err != nil {
			return err
		}
		return store.Save(key, b)
	}
	return access.New(cfg, records, persist)
}

func loadCodes(store driver.Store, key string) ([]access.Record, error) {
	data, found, err := store.Load(key)
	if err != nil {
		return nil, fmt.Errorf("hub: loading the access codes: %w", err)
	}
	if !found {
		return nil, nil
	}
	var records []access.Record
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("hub: reading the access codes: %w", err)
	}
	return records, nil
}

// errReadOnly: the hub only reads the codes; the installer's tools change them.
var errReadOnly = errors.New("hub: the access codes are changed by the installer's tools only")

// openPeopleCodes opens the people's codes for the hub itself: those of the site's
// contacts only. It never writes them, so a code left aside stays on disk for the
// installer to see and remove.
func (h *Hub) openPeopleCodes() (*access.Registry, error) {
	records, err := loadCodes(h.deps.Store, keyAccess)
	if err != nil {
		return nil, err
	}
	return access.New(h.cfg.Access, h.contactsOnly(records), func([]access.Record) error { return errReadOnly })
}

// contactsOnly leaves aside the codes of users who are not contacts of the site: a
// person removed from the contacts (by the cloud's configuration) must no longer
// disarm, nor take one of the places. The code itself is never logged.
func (h *Hub) contactsOnly(records []access.Record) []access.Record {
	return slices.DeleteFunc(records, func(r access.Record) bool {
		if slices.ContainsFunc(h.cfg.Escalation.Contacts, func(c escalation.Contact) bool { return c.ID == r.User }) {
			return false
		}
		h.log.Warn("access code left aside: its user is not a contact of the site (remove it from the console, or: safehome-hub codes remove)", "user", r.User)
		return true
	})
}

// ReloadCodes reads the saved codes again. The installer's tools write them from
// another process, and the composition root calls this when told to (SIGHUP). A
// damaged file leaves the codes in use untouched and is reported.
func (h *Hub) ReloadCodes() error {
	records, err := loadCodes(h.deps.Store, keyAccess)
	if err != nil {
		return err
	}
	installer, err := loadCodes(h.deps.Store, keyInstaller)
	if err != nil {
		return err
	}
	records = h.contactsOnly(records)
	if err := h.codes.Reload(records); err != nil {
		return err
	}
	if err := h.installer.Reload(installer); err != nil {
		return err
	}
	h.log.Info("access codes reloaded", "users", len(records), "installer", len(installer) > 0)
	return nil
}
