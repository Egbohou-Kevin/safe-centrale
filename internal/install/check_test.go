package install

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"safehome/internal/contract"
	"safehome/internal/engine"
)

func dev(id string, caps ...string) contract.RegistryDevice {
	return contract.RegistryDevice{DeviceID: id, Model: "model." + id, Capabilities: caps, Adapter: contract.AdapterZigbee}
}

func TestEveryEngineZoneKindHasItsSensors(t *testing.T) {
	want := []string{
		string(engine.ZoneFire), string(engine.ZonePanic), string(engine.ZoneTamper),
		string(engine.ZoneEntry), string(engine.ZoneInstant), string(engine.ZoneInterior),
	}
	slices.Sort(want)
	if got := ZoneKinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("zone kinds %v, engine has %v", got, want)
	}
}

func TestCanServe(t *testing.T) {
	for _, c := range []struct {
		kind string
		caps []string
		want error // nil: it can; errAny: refused, not as ErrCannotServe
	}{
		{"entry", []string{"contact", "battery"}, nil},
		{"instant", []string{"vibration"}, nil},
		{"interior", []string{"motion", "battery"}, nil},
		{"24h_fire", []string{"smoke", "battery"}, nil},
		{"24h_panic", []string{"panic"}, nil},
		{"24h_tamper", []string{"contact", "tamper"}, nil},
		{"24h_fire", []string{"contact", "battery"}, ErrCannotServe},
		{"entry", []string{"siren", "mains"}, ErrCannotServe},
		{"24h_panic", []string{"keypad"}, ErrCannotServe},
		{"interior", []string{"glass_break"}, ErrCannotServe},
		{"garage", []string{"contact"}, errAny},
	} {
		checkRefusal(t, fmt.Sprintf("CanServe(%s, %v)", c.kind, c.caps), CanServe(c.kind, c.caps), c.want)
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]error{
		"Entree":                                nil,
		"Porte d'entr\u00e9e":                   nil,
		"":                                      ErrEmptyName,
		"   ":                                   ErrEmptyName,
		"Vitrine\n":                             errAny,
		"Porte\u202eArriere":                    errAny, // bidi override: shows the name backwards
		"Porte\u200bArriere":                    errAny, // zero-width space
		strings.Repeat("a", MaxNameLength):      nil,
		strings.Repeat("\u00e9", MaxNameLength): nil,
		strings.Repeat("a", MaxNameLength+1):    ErrNameTooLong,
	} {
		checkRefusal(t, fmt.Sprintf("ValidName(%q)", name), ValidName(name), want)
	}
}

// errAny stands for a refusal that has no sentinel of its own.
var errAny = errors.New("any refusal")

func checkRefusal(t *testing.T, call string, err, want error) {
	t.Helper()
	switch {
	case want == nil && err != nil:
		t.Errorf("%s = %v, want nil", call, err)
	case want == errAny && err == nil:
		t.Errorf("%s = nil, want a refusal", call)
	case want != nil && want != errAny && !errors.Is(err, want):
		t.Errorf("%s = %v, want %v", call, err, want)
	}
}

func TestCheckOfAGoodSiteSaysNothing(t *testing.T) {
	site := Site{
		Zones:    map[string]Zone{"dev_door": {"Entree", "entry"}, "dev_smoke": {"Fumee", "24h_fire"}},
		SirenIDs: []string{"dev_siren1"},
	}
	devices := []contract.RegistryDevice{
		dev("dev_smoke", "smoke", "battery"), dev("dev_door", "contact", "battery"),
		dev("dev_siren1", "siren", "mains"), dev("dev_keypad", "keypad", "battery"), // a keypad needs no zone
	}
	before := slices.Clone(devices)
	if f := Check(site, devices); len(f) != 0 {
		t.Fatalf("findings on a good site: %v", f)
	}
	if !reflect.DeepEqual(devices, before) {
		t.Fatal("Check reordered the caller's devices")
	}
}

func TestCheckFindsWhatIsWrong(t *testing.T) {
	site := Site{
		Zones: map[string]Zone{
			"dev_door":   {"Entree", "entry"},    // fine
			"dev_smoke":  {"Fumee", "entry"},     // a smoke detector cannot trigger an entry zone
			"dev_gone":   {"Reserve", "instant"}, // not paired
			"dev_window": {"Vitrine", "glass"},   // unknown kind
			"dev_noname": {"", "instant"},        // no name
		},
		SirenIDs:   []string{"dev_motion", "dev_lost"},
		SirenNames: map[string]string{"dev_lost": "Reserve\t"}, // a tab in a name
	}
	devices := []contract.RegistryDevice{
		dev("dev_door", "contact"), dev("dev_smoke", "smoke"), dev("dev_window", "contact"),
		dev("dev_noname", "contact"), dev("dev_motion", "motion"), dev("dev_spare", "contact"),
	}
	var got []string
	for _, f := range Check(site, devices) {
		got = append(got, string(f.Level)+" "+f.DeviceID)
	}
	want := []string{
		"warning dev_gone",
		"problem dev_noname",
		"problem dev_smoke",
		"problem dev_window",
		"problem dev_motion", // a siren that cannot sound
		"warning dev_motion", // one of two sirens, without a name
		"warning dev_lost",   // a siren that is not paired
		"problem dev_lost",   // a siren's name with a control character
		"warning dev_spare",  // paired, used for nothing
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !HasProblem(Check(site, devices)) {
		t.Fatal("HasProblem missed the problems")
	}
	if f := Check(site, devices)[5]; !strings.Contains(f.Text, "the alerts call it \"Sir\u00e8ne 1\"") {
		t.Fatalf("an unnamed siren must say what the alerts call it: %s", f)
	}
}

// A siren with a zone has its zone's name: unnamed, it is no warning, nor is the only
// other siren, which the alerts call "Sirene"; a name of its own is a problem.
func TestCheckOfASirenWithAZone(t *testing.T) {
	site := Site{Zones: map[string]Zone{"dev_smoke": {"Cuisine", "24h_fire"}}, SirenIDs: []string{"dev_smoke", "dev_siren"}}
	devices := []contract.RegistryDevice{dev("dev_smoke", "smoke", "siren"), dev("dev_siren", "siren")}
	if f := Check(site, devices); len(f) != 0 {
		t.Fatalf("%v", f)
	}
	site.SirenNames = map[string]string{"dev_smoke": "Reserve"}
	if f := Check(site, devices); len(f) != 1 || f[0].Level != Problem || f[0].DeviceID != "dev_smoke" ||
		!strings.Contains(f[0].Text, `the alerts call it "Cuisine"`) {
		t.Fatalf("%v", f)
	}
}

func TestCheckOfAnEmptySite(t *testing.T) {
	f := Check(Site{}, nil)
	if len(f) != 2 || f[0].DeviceID != "" || HasProblem(f) {
		t.Fatalf("%v", f)
	}
	if s := f[0].String(); s != "warning: no zone: nothing is watched" {
		t.Fatalf("%q", s)
	}
	one := Finding{Level: Problem, DeviceID: "dev_a", Text: "x"}
	if one.String() != "problem: dev_a: x" {
		t.Fatalf("%q", one.String())
	}
}
