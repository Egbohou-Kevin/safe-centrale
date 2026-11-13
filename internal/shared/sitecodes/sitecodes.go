// Package sitecodes is the access codes of a site as they are kept on disk, and the
// rule of changing them: only a contact of the site or the installer has a code, one
// code opens one thing, and a code refused as someone else's too often stops the
// changes for a while. The hub reads them, its command line and the local console
// change them: several programs use it, so it lies in shared (docs/rangement-par-
// programme.md, 13/11/2026).
package sitecodes

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"safehome/internal/shared/core/access"
	"safehome/internal/shared/driver"
)

// CodesLock is the lock the installer's tools hold while they change the access codes
// (read, change, write the whole list): the local console's page serves two phones at
// once, and the command-line tool may run meanwhile. Without it, the second write
// would drop the first one's code.
const CodesLock = "codes.lock"

// CodesFolder is the folder of the hub's state folder that keeps the access codes
// (hashed) and their lock: the other services of the hub are kept out of it.
const CodesFolder = "codes"

// CodesDir is the folder of the access codes in the state folder stateDir: the only
// place that makes its path.
func CodesDir(stateDir string) string { return filepath.Join(stateDir, CodesFolder) }

// Codes are the access codes as the installer's tools (the hub's command line, the
// local console) change them, without running a hub: the people's and the
// installer's, with the site's contacts. Every change is saved before it takes
// effect; the running hub reads the codes again when told.
type Codes struct {
	people, installer *access.Registry
	contacts          []Contact
	store             driver.Store
	now               func() time.Time
}

// The keys of the codes in their store: the people's, the installer's, and when a
// code was refused as someone else's (see Set). The hub reads the first two.
const (
	KeyPeople    = "access"
	KeyInstaller = "installer"
	keyTaken     = "codes-taken"
)

// InstallerUser is the user of the installer code. That code is apart from the
// people's codes: it takes none of their places, and never disarms.
const InstallerUser = "installer"

// Contact is a contact of the site, as the codes know it: only a contact (or the
// installer) may have a code.
type Contact struct {
	ID   string
	Name string
	Role string
}

// A code refused as someone else's tells that it opens the site: takenLimit of them
// within takenWindow stop the codes from being set until the window has passed, so
// that the installer's tools cannot be used to try the codes one after another.
const (
	takenLimit  = 3
	takenWindow = 10 * time.Minute
)

// ErrTooManyTaken refuses a code while too many were refused as someone else's.
var ErrTooManyTaken = errors.New("hub: too many codes refused as someone else's, wait 10 minutes")

// ErrNotAContact refuses a code for someone who is neither a contact of the site nor
// the installer: the hub would leave it aside.
var ErrNotAContact = errors.New("hub: not a contact of the site")

// Open opens the codes saved in store, with the settings cfg, for these contacts;
// clock dates the codes refused as someone else's (see Set).
func Open(cfg access.Config, contacts []Contact, store driver.Store, clock driver.Clock) (*Codes, error) {
	people, err := OpenPeople(cfg, store)
	if err != nil {
		return nil, err
	}
	installer, err := OpenInstaller(cfg, store)
	if err != nil {
		return nil, err
	}
	return &Codes{people: people, installer: installer, contacts: contacts, store: store, now: clock.Now}, nil
}

// OpenPeople opens the people's codes saved in store; a change is saved there.
func OpenPeople(cfg access.Config, store driver.Store) (*access.Registry, error) {
	return openRegistry(cfg, store, KeyPeople)
}

// OpenInstaller opens the installer code saved in store: one code at most.
func OpenInstaller(cfg access.Config, store driver.Store) (*access.Registry, error) {
	cfg.MaxUsers = 1
	return openRegistry(cfg, store, KeyInstaller)
}

// CodeHolder is a contact of the site, and whether they have a code.
type CodeHolder struct {
	Contact
	HasCode bool
}

// CodeList says who has a code. It never holds a code.
type CodeList struct {
	Contacts  []CodeHolder // in the configuration's order
	Leftovers []string     // users of codes who are not contacts, sorted: the hub leaves those codes aside
	Installer bool         // the installer code is set
}

// List says who has a code.
func (s *Codes) List() CodeList {
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
func (s *Codes) Set(user, code string) error {
	if user != InstallerUser && !slices.ContainsFunc(s.contacts, func(c Contact) bool { return c.ID == user }) {
		return fmt.Errorf("%w: %s", ErrNotAContact, user)
	}
	taken, err := s.recentTaken()
	if err != nil {
		return err
	}
	if len(taken) >= takenLimit {
		return ErrTooManyTaken
	}
	own, other := s.of(user)
	if _, theirs := other.Verify(code); theirs {
		err = access.ErrCodeTaken
	} else {
		err = own.Set(user, code)
	}
	if errors.Is(err, access.ErrCodeTaken) {
		if serr := s.saveTaken(append(taken, s.now())); serr != nil {
			return serr
		}
	}
	return err
}

// recentTaken returns when codes were refused as someone else's within takenWindow.
func (s *Codes) recentTaken() ([]time.Time, error) {
	data, found, err := s.store.Load(keyTaken)
	if err != nil || !found {
		return nil, err
	}
	var at []time.Time
	if err := json.Unmarshal(data, &at); err != nil {
		return nil, nil // damaged: counted afresh
	}
	now := s.now()
	return slices.DeleteFunc(at, func(t time.Time) bool { return !now.Before(t.Add(takenWindow)) || t.After(now) }), nil
}

func (s *Codes) saveTaken(at []time.Time) error {
	b, err := json.Marshal(at)
	if err != nil {
		return err
	}
	return s.store.Save(keyTaken, b)
}

// Remove removes the code of user (access.ErrNoSuchUser if there is none).
func (s *Codes) Remove(user string) error {
	own, _ := s.of(user)
	return own.Remove(user)
}

// of returns the registry of user, and the other one.
func (s *Codes) of(user string) (own, other *access.Registry) {
	if user == InstallerUser {
		return s.installer, s.people
	}
	return s.people, s.installer
}

func openRegistry(cfg access.Config, store driver.Store, key string) (*access.Registry, error) {
	records, err := Load(store, key)
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

// Load reads the codes saved under key in store (KeyPeople, KeyInstaller): the hub
// reads them so.
func Load(store driver.Store, key string) ([]access.Record, error) {
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
