// Package install is the application layer of the installer's tools: pairing and
// removing devices, giving each one its use (zone or siren), checking that the site's
// configuration agrees with what is paired, and watching devices during the tests.
//
// The command-line tool uses it today, the local web console will later. It knows no
// adapter and no configuration file: the program gives it a publisher and the
// messages, and translates the site from and to the hub's configuration.
package install

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"safehome/internal/contract"
	"safehome/internal/engine"
)

// Zone is what a device is for: a name for people and a kind for the engine.
type Zone struct {
	Name string
	Kind string
}

// Site is the use of the devices: their zones and the sirens.
type Site struct {
	Zones    map[string]Zone
	SirenIDs []string
}

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

// zoneSensors lists, for each kind of zone, the capabilities that can trigger it.
// The engine reacts to any active sensor of a zone's device; a device with none of
// these would sit in its zone and never raise anything.
var zoneSensors = map[engine.ZoneKind][]string{
	engine.ZoneEntry:    {contract.CapContact, contract.CapMotion, contract.CapVibration, contract.CapGlassBreak},
	engine.ZoneInstant:  {contract.CapContact, contract.CapMotion, contract.CapVibration, contract.CapGlassBreak},
	engine.ZoneInterior: {contract.CapMotion, contract.CapContact},
	engine.ZoneFire:     {contract.CapSmoke},
	engine.ZonePanic:    {contract.CapPanic},
	engine.ZoneTamper:   {contract.CapTamper},
}

// ZoneKinds lists the kinds of zone, for help texts.
func ZoneKinds() []string {
	out := make([]string, 0, len(zoneSensors))
	for k := range zoneSensors {
		out = append(out, string(k))
	}
	slices.Sort(out)
	return out
}

// ValidKind checks a kind of zone.
func ValidKind(kind string) error {
	if _, ok := zoneSensors[engine.ZoneKind(kind)]; !ok {
		return fmt.Errorf("unknown zone kind %q (use %s)", kind, strings.Join(ZoneKinds(), ", "))
	}
	return nil
}

// CanServe says whether a device with these capabilities can be used in a zone of
// this kind.
func CanServe(kind string, capabilities []string) error {
	if err := ValidKind(kind); err != nil {
		return err
	}
	sensors := zoneSensors[engine.ZoneKind(kind)]
	for _, c := range capabilities {
		if slices.Contains(sensors, c) {
			return nil
		}
	}
	return fmt.Errorf("a %s zone needs one of %s, the device has %s",
		kind, strings.Join(sensors, ", "), strings.Join(capabilities, ", "))
}

// MaxNameLength bounds a zone name: it goes into SMS and voice messages.
const MaxNameLength = 32

// ValidName checks a zone name.
func ValidName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return fmt.Errorf("empty zone name")
	case utf8.RuneCountInString(name) > MaxNameLength:
		return fmt.Errorf("zone name %q is longer than %d characters", name, MaxNameLength)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return fmt.Errorf("zone name %q contains a control character", name)
	}
	return nil
}

// Check compares the site with the registry of paired devices.
func Check(site Site, devices []contract.RegistryDevice) []Finding {
	devices = slices.Clone(devices) // sorted below
	byID := map[string]contract.RegistryDevice{}
	for _, d := range devices {
		byID[d.DeviceID] = d
	}
	var out []Finding
	add := func(l Level, id, format string, args ...any) {
		out = append(out, Finding{Level: l, DeviceID: id, Text: fmt.Sprintf(format, args...)})
	}

	for _, id := range sortedKeys(site.Zones) {
		z := site.Zones[id]
		if err := ValidName(z.Name); err != nil {
			add(Problem, id, "%v", err)
		}
		d, paired := byID[id]
		if err := ValidKind(z.Kind); err != nil {
			add(Problem, id, "zone %q: %v", z.Name, err)
		} else if paired {
			if err := CanServe(z.Kind, d.Capabilities); err != nil {
				add(Problem, id, "zone %q can never trigger: %v", z.Name, err)
			}
		}
		if !paired {
			add(Warning, id, "zone %q: the device is not paired (removed, misspelt, or from another adapter)", z.Name)
		}
	}
	for _, id := range site.SirenIDs {
		d, paired := byID[id]
		switch {
		case !paired:
			add(Warning, id, "siren: the device is not paired (removed, misspelt, or from another adapter)")
		case !slices.Contains(d.Capabilities, contract.CapSiren):
			add(Problem, id, "listed as a siren but cannot sound (it has %s)", strings.Join(d.Capabilities, ", "))
		}
	}
	slices.SortFunc(devices, func(a, b contract.RegistryDevice) int { return strings.Compare(a.DeviceID, b.DeviceID) })
	for _, d := range devices {
		_, zoned := site.Zones[d.DeviceID]
		if zoned || slices.Contains(site.SirenIDs, d.DeviceID) || slices.Contains(d.Capabilities, contract.CapKeypad) {
			continue
		}
		add(Warning, d.DeviceID, "paired (%s) but used for nothing: give it a zone, make it a siren, or remove it", d.Model)
	}
	if len(site.Zones) == 0 {
		add(Warning, "", "no zone: nothing is watched")
	}
	if len(site.SirenIDs) == 0 {
		add(Warning, "", "no siren: an alarm will only send messages")
	}
	return out
}

// HasProblem says whether a finding must be fixed.
func HasProblem(findings []Finding) bool {
	return slices.ContainsFunc(findings, func(f Finding) bool { return f.Level == Problem })
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
