// Package site is the use of the devices of a site: their zones and the sirens, and
// the rule of the names people read in the alerts. The installer's tools edit it
// (install), the hub's configuration keeps it on disk (hubconfig): both programs use
// it, so it lies in shared (docs/rangement-par-programme.md, 12/11/2026).
package site

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"safehome/internal/shared/contract"
)

// Zone is what a device is for: a name for people and a kind for the engine, and
// where it is, the room ("Salon"), for the owner's list of devices; empty when the
// installer gave none. Same fields, in the same order, as the configuration file's
// zones (hubconfig converts one into the other).
type Zone struct {
	Name string
	Kind string
	Room string
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
		SirenNames: Unnamed(s.SirenNames, id)}
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
	plain := s.PlainSirens()
	rank := slices.Index(plain, id)
	if len(plain) < 2 || rank < 0 {
		return defaultSirenName
	}
	return fmt.Sprintf("%s %d", defaultSirenName, rank+1)
}

// PlainSirens lists the sirens without a zone, in the order they were added: the
// ones a name, or a default name, is for.
func (s Site) PlainSirens() []string {
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

// Unnamed returns a copy of names without the siren id.
func Unnamed(names map[string]string, id string) map[string]string {
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

// ErrHasZone refuses a name for a siren with a zone.
var ErrHasZone = errors.New("has a zone: the alerts call it by its zone's name")

// The refusals of a zone or of a siren's name, for a tool to explain them.
var (
	ErrEmptyName   = errors.New("is empty")
	ErrNameTooLong = fmt.Errorf("longer than %d characters", MaxNameLength)
)

// MaxNameLength bounds a zone's or a siren's name: it goes into SMS and voice
// messages, and a zone's names the device in the alarm events (device_name).
const MaxNameLength = contract.MaxDeviceName

// ValidName checks a zone name. It refuses control characters (a line break) and
// invisible format characters (a bidi override would show the name backwards in an
// SMS or on the phone).
func ValidName(name string) error { return validName("zone", name) }

// ValidSirenName checks a siren's name, by the zones' rule: it goes into the same
// texts.
func ValidSirenName(name string) error { return validName("siren", name) }

// ValidRoom checks a zone's room, by the zones' rule: written by the installer, it
// goes to the cloud and the app. Empty is no room, and is valid.
func ValidRoom(room string) error {
	if room == "" {
		return nil
	}
	return validName("room", room)
}

func validName(what, name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return fmt.Errorf("%s name %w", what, ErrEmptyName)
	case utf8.RuneCountInString(name) > MaxNameLength:
		return fmt.Errorf("%s name %q is %w", what, name, ErrNameTooLong)
	case strings.IndexFunc(name, invisible) >= 0:
		return fmt.Errorf("%s name %q contains a control or invisible character", what, name)
	}
	return nil
}

func invisible(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }
