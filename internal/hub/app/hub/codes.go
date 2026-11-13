package hub

import (
	"errors"
	"slices"

	"safehome/internal/shared/core/access"
	"safehome/internal/shared/driver"
	"safehome/internal/shared/sitecodes"
)

// SiteCodes are the access codes as the installer's tools change them
// (sitecodes.Codes); CodeList says who has one.
type (
	SiteCodes = sitecodes.Codes
	CodeList  = sitecodes.CodeList
)

// The refusals of a code (sitecodes).
var (
	ErrTooManyTaken = sitecodes.ErrTooManyTaken
	ErrNotAContact  = sitecodes.ErrNotAContact
)

// OpenSiteCodes opens the codes saved in store, for the contacts of cfg
// (sitecodes.Open).
func OpenSiteCodes(cfg Config, store driver.Store, clock driver.Clock) (*SiteCodes, error) {
	return sitecodes.Open(cfg.Access, CodeContacts(cfg), store, clock)
}

// CodeContacts are the contacts of cfg, as the codes know them.
func CodeContacts(cfg Config) []sitecodes.Contact {
	out := make([]sitecodes.Contact, 0, len(cfg.Escalation.Contacts))
	for _, c := range cfg.Escalation.Contacts {
		out = append(out, sitecodes.Contact{ID: c.ID, Name: c.Name, Role: string(c.Role)})
	}
	return out
}

// errReadOnly: the hub only reads the codes; the installer's tools change them.
var errReadOnly = errors.New("hub: the access codes are changed by the installer's tools only")

// openPeopleCodes opens the people's codes for the hub itself: those of the site's
// contacts only. It never writes them, so a code left aside stays on disk for the
// installer to see and remove.
func (h *Hub) openPeopleCodes() (*access.Registry, error) {
	records, err := sitecodes.Load(h.codesStore(), sitecodes.KeyPeople)
	if err != nil {
		return nil, err
	}
	return access.New(h.cfg.Access, h.contactsOnly(records), func([]access.Record) error { return errReadOnly })
}

// codesStore is where the hub reads the access codes.
func (h *Hub) codesStore() driver.Store {
	if h.deps.Codes != nil {
		return h.deps.Codes
	}
	return h.deps.Store
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
	records, err := sitecodes.Load(h.codesStore(), sitecodes.KeyPeople)
	if err != nil {
		return err
	}
	installer, err := sitecodes.Load(h.codesStore(), sitecodes.KeyInstaller)
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
