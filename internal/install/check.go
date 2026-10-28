// Package install is the application layer of the installer's tools: pairing and
// removing devices, giving each one its use (zone or siren), checking that the site's
// configuration agrees with what is paired, and watching devices during the tests.
//
// The command-line tool (cmd/safehome-install) and the local console
// (internal/consoleapp) both sit on it. It knows no adapter and no configuration file:
// the program gives it a publisher and the messages (Session), and the site's files
// behind the Sites port (Editor).
package install

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"safehome/internal/contract"
	"safehome/internal/engine"
)

// Level says how serious a finding is.
type Level string

const (
	// Problem: the site does not work as the installer believes (a zone that can
	// never trigger, a siren that cannot sound). To fix before handing over.
	Problem Level = "problem"
	// Warning: worth a look, may be intended.
	Warning Level = "warning"
)

// Finding is one remark of Check.
type Finding struct {
	Level    Level
	DeviceID string // empty for a remark about the whole site
	Text     string
}

func (f Finding) String() string {
	if f.DeviceID == "" {
		return fmt.Sprintf("%s: %s", f.Level, f.Text)
	}
	return fmt.Sprintf("%s: %s: %s", f.Level, f.DeviceID, f.Text)
}

// ZoneKinds lists the kinds of zone, for help texts. The engine owns them, with the
// sensors each kind reacts to.
func ZoneKinds() []string {
	var out []string
	for _, k := range engine.ZoneKinds() {
		out = append(out, string(k))
	}
	return out
}

// ValidKind checks a kind of zone.
func ValidKind(kind string) error {
	if !slices.Contains(engine.ZoneKinds(), engine.ZoneKind(kind)) {
		return fmt.Errorf("unknown zone kind %q (use %s)", kind, strings.Join(ZoneKinds(), ", "))
	}
	return nil
}

// The refusals of a zone or of a siren's name, for a tool to explain them.
var (
	ErrEmptyName   = errors.New("is empty")
	ErrNameTooLong = fmt.Errorf("longer than %d characters", MaxNameLength)
	ErrCannotServe = errors.New("the device cannot trigger this kind of zone")
)

// CanServe says whether a device with these capabilities can be used in a zone of
// this kind.
func CanServe(kind string, capabilities []string) error {
	if err := ValidKind(kind); err != nil {
		return err
	}
	// A device with none of the sensors its zone reacts to would sit in that zone
	// and never raise anything. Sensor kinds are spelled like the capabilities.
	for _, c := range capabilities {
		if engine.Accepts(engine.ZoneKind(kind), engine.SensorKind(c)) {
			return nil
		}
	}
	var sensors []string
	for _, s := range engine.ZoneKind(kind).Sensors() {
		sensors = append(sensors, string(s))
	}
	return fmt.Errorf("%w: a %s zone needs one of %s, the device has %s",
		ErrCannotServe, kind, strings.Join(sensors, ", "), strings.Join(capabilities, ", "))
}

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

// Check compares the site with the registry of paired devices.
func Check(site Site, devices []contract.RegistryDevice) []Finding {
	byID := make(map[string]contract.RegistryDevice, len(devices))
	for _, d := range devices {
		byID[d.DeviceID] = d
	}
	var f findings
	f.zones(site, byID)
	f.sirens(site, byID)
	f.unused(site, devices)
	if len(site.Zones) == 0 {
		f.add(Warning, "", "no zone: nothing is watched")
	}
	if len(site.SirenIDs) == 0 {
		f.add(Warning, "", "no siren: an alarm will only send messages")
	} else if !slices.ContainsFunc(site.SirenIDs, func(id string) bool { _, ok := byID[id]; return ok }) {
		f.add(Problem, "", "no siren can sound: none of the sirens is paired (removed meanwhile?)")
	}
	return f
}

// findings collects the remarks of Check, in order.
type findings []Finding

func (f *findings) add(l Level, id, format string, args ...any) {
	*f = append(*f, Finding{Level: l, DeviceID: id, Text: fmt.Sprintf(format, args...)})
}

// zones checks each zone: a valid name and kind, a paired device that can trigger it.
func (f *findings) zones(site Site, byID map[string]contract.RegistryDevice) {
	for _, id := range slices.Sorted(maps.Keys(site.Zones)) {
		z := site.Zones[id]
		if err := ValidName(z.Name); err != nil {
			f.add(Problem, id, "%v", err)
		}
		if err := ValidRoom(z.Room); err != nil {
			f.add(Problem, id, "zone %q: %v", z.Name, err)
		}
		d, paired := byID[id]
		if err := ValidKind(z.Kind); err != nil {
			f.add(Problem, id, "zone %q: %v", z.Name, err)
		} else if paired {
			if err := CanServe(z.Kind, d.Capabilities); err != nil {
				f.add(Problem, id, "zone %q can never trigger: %v", z.Name, err)
			}
		}
		if !paired {
			f.add(Warning, id, "zone %q: the device is not paired (removed, misspelt, or from another adapter)", z.Name)
		}
	}
}

// sirens checks that each siren is paired and can sound, and how the alerts name it:
// among several sirens without a zone, one without a name is only known by its rank.
// A siren with a zone has its zone's name.
func (f *findings) sirens(site Site, byID map[string]contract.RegistryDevice) {
	plain := site.plainSirens()
	for _, id := range site.SirenIDs {
		d, paired := byID[id]
		switch {
		case !paired:
			f.add(Warning, id, "siren: the device is not paired (removed, misspelt, or from another adapter)")
		case !slices.Contains(d.Capabilities, contract.CapSiren):
			f.add(Problem, id, "listed as a siren but cannot sound (it has %s)", strings.Join(d.Capabilities, ", "))
		}
		name, named := site.SirenNames[id]
		switch {
		case named:
			if err := ValidSirenName(name); err != nil {
				f.add(Problem, id, "%v", err)
			} else if _, zoned := site.Zones[id]; zoned {
				f.add(Problem, id, "siren named %q, but it has a zone: the alerts call it %q, by its zone's name", name, site.SirenName(id))
			}
		case len(plain) > 1 && slices.Contains(plain, id):
			f.add(Warning, id, "siren without a name: the alerts call it %q, give it one", site.SirenName(id))
		}
	}
}

// unused lists the paired devices given no use. A keypad needs none.
func (f *findings) unused(site Site, devices []contract.RegistryDevice) {
	devices = slices.Clone(devices) // the caller's order is kept
	slices.SortFunc(devices, func(a, b contract.RegistryDevice) int { return strings.Compare(a.DeviceID, b.DeviceID) })
	for _, d := range devices {
		_, zoned := site.Zones[d.DeviceID]
		if zoned || slices.Contains(site.SirenIDs, d.DeviceID) || slices.Contains(d.Capabilities, contract.CapKeypad) {
			continue
		}
		f.add(Warning, d.DeviceID, "paired (%s) but used for nothing: give it a zone, make it a siren, or remove it", d.Model)
	}
}

// HasProblem says whether a finding must be fixed.
func HasProblem(findings []Finding) bool {
	return slices.ContainsFunc(findings, func(f Finding) bool { return f.Level == Problem })
}
