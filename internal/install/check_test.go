package install

import (
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
		ok   bool
	}{
		{"entry", []string{"contact", "battery"}, true},
		{"instant", []string{"vibration"}, true},
		{"interior", []string{"motion", "battery"}, true},
		{"24h_fire", []string{"smoke", "battery"}, true},
		{"24h_panic", []string{"panic"}, true},
		{"24h_tamper", []string{"contact", "tamper"}, true},
		{"24h_fire", []string{"contact", "battery"}, false},
		{"entry", []string{"siren", "mains"}, false},
		{"24h_panic", []string{"keypad"}, false},
		{"interior", []string{"glass_break"}, false},
		{"garage", []string{"contact"}, false},
	} {
		if err := CanServe(c.kind, c.caps); (err == nil) != c.ok {
			t.Errorf("CanServe(%s, %v) = %v, want ok=%v", c.kind, c.caps, err, c.ok)
		}
	}
}

func TestValidName(t *testing.T) {
	for name, ok := range map[string]bool{
		"Entree":                                true,
		"Porte d'entr\u00e9e":                   true,
		"":                                      false,
		"   ":                                   false,
		"Vitrine\n":                             false,
		strings.Repeat("a", MaxNameLength):      true,
		strings.Repeat("\u00e9", MaxNameLength): true,
		strings.Repeat("a", MaxNameLength+1):    false,
	} {
		if err := ValidName(name); (err == nil) != ok {
			t.Errorf("ValidName(%q) = %v, want ok=%v", name, err, ok)
		}
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
		SirenIDs: []string{"dev_motion", "dev_lost"},
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
		"warning dev_lost",   // a siren that is not paired
		"warning dev_spare",  // paired, used for nothing
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !HasProblem(Check(site, devices)) {
		t.Fatal("HasProblem missed the problems")
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
