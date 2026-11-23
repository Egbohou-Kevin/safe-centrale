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
// installer's and the duress codes, with the site's contacts. Every change is saved before it takes
// effect; the running hub reads the codes again when told.
type Codes struct {
	people, installer, duress *access.Registry
	contacts                  []Contact
	store                     driver.Store
	now                       func() time.Time
}

// The keys of the codes in their store: the people's, the installer's, the duress
// codes, and when a code was refused as someone else's (see Set). The hub reads the
// first three.
const (
	KeyPeople    = "access"
	KeyInstaller = "installer"
	KeyDuress    = "duress"
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

// ErrNoPersonCode refuses a duress code for a contact who has no code of their own:
// a duress code disarms, it is never someone's only access.
var ErrNoPersonCode = errors.New("hub: no code of their own")

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
	duress, err := OpenDuress(cfg, store)
	if err != nil {
		return nil, err
	}
	return &Codes{people: people, installer: installer, duress: duress, contacts: contacts, store: store, now: clock.Now}, nil
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

// OpenDuress opens the duress codes saved in store: a contact made to disarm under
// threat types theirs instead of their own code. It disarms the same, and tells the
// hub (engine.Auth.Duress). A registry apart, as the installer's: a Record holds no
// kind, and a registry holds one code per user (access.Registry.Set). At most one
// per contact, as many places as the people's codes.
func OpenDuress(cfg access.Config, store driver.Store) (*access.Registry, error) {
	return openRegistry(cfg, store, KeyDuress)
}

// CodeHolder is a contact of the site, and whether they have a code.
type CodeHolder struct {
	Contact
	HasCode   bool
	HasDuress bool // a duress code is set too
}

// CodeList says who has a code. It never holds a code.
type CodeList struct {
	Contacts  []CodeHolder // in the configuration's order
	Leftovers []string     // users of codes who are not contacts, sorted: the hub leaves those codes aside
	Installer bool         // the installer code is set

	DuressLeftovers []string // users of duress codes who are not contacts, sorted: left aside the same
}

// List says who has a code.
func (s *Codes) List() CodeList {
	have, duress := map[string]bool{}, map[string]bool{}
	for _, u := range s.people.Users() {
		have[u] = true
	}
	for _, u := range s.duress.Users() {
		duress[u] = true
	}
	var l CodeList
	for _, ct := range s.contacts {
		l.Contacts = append(l.Contacts, CodeHolder{Contact: ct, HasCode: have[ct.ID], HasDuress: duress[ct.ID]})
		delete(have, ct.ID)
		delete(duress, ct.ID)
	}
	l.Leftovers = slices.Sorted(maps.Keys(have))
	l.DuressLeftovers = slices.Sorted(maps.Keys(duress))
	l.Installer = len(s.installer.Users()) > 0
	return l
}

// Set gives user, a contact of the site or the installer, a code. One code opens one
// thing: a code is in one registry only, people's, installer's or duress
// (access.ErrCodeTaken). The code is never in an error.
func (s *Codes) Set(user, code string) error {
	if user != InstallerUser && !s.contact(user) {
		return fmt.Errorf("%w: %s", ErrNotAContact, user)
	}
	own, others := s.of(user)
	return s.set(own, others, user, code)
}

// SetDuress gives user, a contact of the site who has a code, a duress code: never
// the installer, never someone without access (ErrNoPersonCode). It may be neither a
// person's code (theirs included) nor the installer's.
func (s *Codes) SetDuress(user, code string) error {
	if !s.contact(user) {
		return fmt.Errorf("%w: %s", ErrNotAContact, user)
	}
	if !slices.Contains(s.people.Users(), user) {
		return fmt.Errorf("%w: %s", ErrNoPersonCode, user)
	}
	return s.set(s.duress, []*access.Registry{s.people, s.installer}, user, code)
}

func (s *Codes) contact(user string) bool {
	return slices.ContainsFunc(s.contacts, func(c Contact) bool { return c.ID == user })
}

// set gives user the code in own, unless it is in one of others.
func (s *Codes) set(own *access.Registry, others []*access.Registry, user, code string) error {
	taken, err := s.recentTaken()
	if err != nil {
		return err
	}
	if len(taken) >= takenLimit {
		return ErrTooManyTaken
	}
	if slices.ContainsFunc(others, func(r *access.Registry) bool { _, theirs := r.Verify(code); return theirs }) {
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

// Remove removes the code of user (access.ErrNoSuchUser if there is none), and for a
// person their duress code too: a person whose access is removed must no longer
// disarm in any way. The duress code goes first, so that a failure never leaves it
// without the person's code on disk (the hub leaves such a one aside anyway).
func (s *Codes) Remove(user string) error {
	own, _ := s.of(user)
	if own == s.installer {
		return own.Remove(user)
	}
	derr := s.duress.Remove(user)
	if derr != nil && !errors.Is(derr, access.ErrNoSuchUser) {
		return derr
	}
	err := own.Remove(user)
	if errors.Is(err, access.ErrNoSuchUser) && derr == nil {
		return nil // only a duress code was left
	}
	return err
}

// RemoveDuress removes the duress code of user (access.ErrNoSuchUser if there is none).
func (s *Codes) RemoveDuress(user string) error { return s.duress.Remove(user) }

// of returns the registry of user's code, and the other ones.
func (s *Codes) of(user string) (own *access.Registry, others []*access.Registry) {
	if user == InstallerUser {
		return s.installer, []*access.Registry{s.people, s.duress}
	}
	return s.people, []*access.Registry{s.installer, s.duress}
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

// Load reads the codes saved under key in store (KeyPeople, KeyInstaller, KeyDuress): the hub
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
