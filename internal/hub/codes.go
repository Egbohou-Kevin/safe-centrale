package hub

import (
	"encoding/json"
	"fmt"

	"safehome/internal/access"
	"safehome/internal/driver"
)

// OpenCodes opens the access codes saved in store. The hub uses it at start, and so
// does the installer's command-line tool, which sets codes without running a hub.
// Every change is saved before it takes effect.
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

// Codes returns the access code registry, for the installer's tools.
func (h *Hub) Codes() *access.Registry { return h.codes }

// ReloadCodes reads the saved codes again. The installer's tool writes them from
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
	if err := h.codes.Reload(records); err != nil {
		return err
	}
	if err := h.installer.Reload(installer); err != nil {
		return err
	}
	h.log.Info("access codes reloaded", "users", len(records), "installer", len(installer) > 0)
	return nil
}
