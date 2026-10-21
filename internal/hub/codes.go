package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"safehome/internal/access"
	"safehome/internal/driver"
	"safehome/internal/escalation"
)

// SiteCodes are the access codes as the installer's tools (the command-line tool,
// the local console) change them, without running a hub: the people's and the
// installer's, with the site's contacts. Every change is saved before it takes
// effect; the running hub reads the codes again when told (ReloadCodes).
type SiteCodes struct {
	people, installer *access.Registry
	contacts          []escalation.Contact
}

// ErrNotAContact refuses a code for someone who is neither a contact of the site nor
// the installer: the hub would leave it aside.
var ErrNotAContact = errors.New("hub: not a contact of the site")

// OpenSiteCodes opens the codes saved in store, for the contacts of cfg.
func OpenSiteCodes(cfg Config, store driver.Store) (*SiteCodes, error) {
	people, err := openCodes(cfg.Access, store)
	if err != nil {
		return nil, err
	}
	installer, err := openInstallerCode(cfg.Access, store)
	if err != nil {
		return nil, err
	}
	return &SiteCodes{people: people, installer: installer, contacts: cfg.Escalation.Contacts}, nil
}

// openCodes opens the people's codes saved in store.
func openCodes(cfg access.Config, store driver.Store) (*access.Registry, error) {
	return openRegistry(cfg, store, keyAccess)
}

// openInstallerCode opens the installer code saved in store: one code at most.
func openInstallerCode(cfg access.Config, store driver.Store) (*access.Registry, error) {
	cfg.MaxUsers = 1
	return openRegistry(cfg, store, keyInstaller)
}

// CodeHolder is a contact of the site, and whether they have a code.
type CodeHolder struct {
	escalation.Contact
	HasCode bool
}

// CodeList says who has a code. It never holds a code.
type CodeList struct {
	Contacts  []CodeHolder // in the configuration's order
	Leftovers []string     // users of codes who are not contacts, sorted: the hub leaves those codes aside
	Installer bool         // the installer code is set
}

// List says who has a code.
func (s *SiteCodes) List() CodeList {
	have := map[string]bool{}
	for _, u := range s.people.Users() {
		have[u] = true
	}
	var l CodeList
	for _, ct := range s.contacts {
		l.Contacts = append(l.Contacts, CodeHolder{Contact: ct, HasCode: have[ct.ID]})
		delete(have, ct.ID)
	}
	l.Leftovers = slices.Sorted(maps.Keys(have))
	l.Installer = len(s.installer.Users()) > 0
	return l
}

// Set gives user, a contact of the site or the installer, a code. One code opens one
// thing: a person's code may not be the installer's, nor the reverse
// (access.ErrCodeTaken). The code is never in an error.
func (s *SiteCodes) Set(user, code string) error {
	if user != InstallerUser && !slices.ContainsFunc(s.contacts, func(c escalation.Contact) bool { return c.ID == user }) {
		return fmt.Errorf("%w: %s", ErrNotAContact, user)
	}
	own, other := s.of(user)
	if _, taken := other.Verify(code); taken {
		return access.ErrCodeTaken
	}
	return own.Set(user, code)
}

// Remove removes the code of user (access.ErrNoSuchUser if there is none).
func (s *SiteCodes) Remove(user string) error {
	own, _ := s.of(user)
	return own.Remove(user)
}

// of returns the registry of user, and the other one.
func (s *SiteCodes) of(user string) (own, other *access.Registry) {
	if user == InstallerUser {
		return s.installer, s.people
	}
	return s.people, s.installer
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
	contacts := h.contactIDs()
	return slices.DeleteFunc(records, func(r access.Record) bool {
		if slices.Contains(contacts, r.User) {
			return false
		}
		h.log.Warn("access code left aside: its user is not a contact of the site (remove it from the console, or: safehome-hub codes remove)", "user", r.User)
		return true
	})
}

// contactIDs returns the site's contacts as saved now, or, when they cannot be read,
// those the hub started with.
func (h *Hub) contactIDs() []string {
	if h.deps.Contacts != nil {
		ids, err := h.deps.Contacts()
		if err == nil {
			return ids
		}
		h.log.Error("reading the site's contacts failed, the codes follow those of the start", "err", err)
	}
	ids := make([]string, 0, len(h.cfg.Escalation.Contacts))
	for _, c := range h.cfg.Escalation.Contacts {
		ids = append(ids, c.ID)
	}
	return ids
}

// ReloadCodes reads the saved codes again, and the contacts they belong to. The
// installer's tools write the codes from another process, and the composition root
// calls this when told to (SIGHUP), and when a new configuration of the site is saved.
// A damaged file leaves the codes in use untouched and is reported.
func (h *Hub) ReloadCodes() error {
	h.reloading.Lock()
	defer h.reloading.Unlock()
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
