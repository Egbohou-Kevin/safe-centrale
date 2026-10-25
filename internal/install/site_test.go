package install

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"safehome/internal/contract"
)

// memSites keeps the site in memory, and refuses one without siren as the hub does.
type memSites struct {
	mu             sync.Mutex
	site           Site
	writes, checks int
}

func (m *memSites) Read() (Site, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Site{Zones: maps.Clone(m.site.Zones), SirenIDs: slices.Clone(m.site.SirenIDs), SirenNames: maps.Clone(m.site.SirenNames)}, nil
}

func (m *memSites) Check(s Site) error {
	m.mu.Lock()
	m.checks++
	m.mu.Unlock()
	if len(s.SirenIDs) == 0 {
		return errors.New("hub: at least one siren is required")
	}
	return nil
}

func (m *memSites) Write(s Site) error {
	if err := m.Check(s); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.site, m.writes = s, m.writes+1
	return nil
}

func newSites() *memSites {
	return &memSites{site: Site{Zones: map[string]Zone{"dev_door": {Name: "Entree", Kind: "entry"}}, SirenIDs: []string{"dev_siren"}}}
}

func registered(id string, caps ...string) *contract.RegistryDevice {
	return &contract.RegistryDevice{DeviceID: id, Capabilities: caps}
}

func TestSetZoneChecksTheZoneAndTheDevice(t *testing.T) {
	sites := newSites()
	e := NewEditor(sites)
	for name, c := range map[string]struct {
		z    Zone
		d    *contract.RegistryDevice
		want error
	}{
		"no name":       {Zone{Name: " ", Kind: "entry"}, nil, ErrEmptyName},
		"long name":     {Zone{Name: strings.Repeat("x", 33), Kind: "entry"}, nil, ErrNameTooLong},
		"cannot serve":  {Zone{Name: "Fumee", Kind: "24h_fire"}, registered("dev_k7p2", "contact"), ErrCannotServe},
		"unknown kind":  {Zone{Name: "Garage", Kind: "garage"}, nil, nil},
		"forced device": {Zone{Name: "Coffre", Kind: "24h_tamper"}, nil, nil},
	} {
		_, err := e.SetZone("dev_k7p2", c.z, c.d)
		switch {
		case name == "unknown kind":
			if err == nil || !strings.Contains(err.Error(), "unknown zone kind") {
				t.Errorf("%s: %v", name, err)
			}
		case !errors.Is(err, c.want):
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	old, err := e.SetZone("dev_door", Zone{Name: "Porte", Kind: "instant"}, registered("dev_door", "contact"))
	if err != nil || old == nil || *old != (Zone{Name: "Entree", Kind: "entry"}) {
		t.Fatalf("%v %v", old, err)
	}
	if s, _ := sites.Read(); s.Zones["dev_door"] != (Zone{Name: "Porte", Kind: "instant"}) || s.Zones["dev_k7p2"].Kind != "24h_tamper" {
		t.Fatalf("%+v", s)
	}
}

func TestUnzone(t *testing.T) {
	sites := newSites()
	e := NewEditor(sites)
	if z, err := e.Unzone("dev_door"); err != nil || z.Name != "Entree" {
		t.Fatalf("%v %v", z, err)
	}
	if _, err := e.Unzone("dev_door"); !errors.Is(err, ErrNoZone) || err.Error() != "dev_door has no zone" {
		t.Fatalf("%v", err)
	}
}

func TestSirens(t *testing.T) {
	sites := newSites()
	e := NewEditor(sites)
	if _, err := e.AddSiren("dev_door", registered("dev_door", "contact", "battery")); !errors.Is(err, ErrCannotSound) ||
		err.Error() != "dev_door cannot sound (it has contact, battery)" {
		t.Fatalf("%v", err)
	}
	if added, err := e.AddSiren("dev_siren", nil); added || err != nil || sites.writes != 0 {
		t.Fatalf("already one: %v %v, %d write(s)", added, err, sites.writes)
	}
	if removed, err := e.RemoveSiren("dev_siren"); removed || !errors.Is(err, ErrLastSiren) {
		t.Fatalf("the last one: %v %v", removed, err)
	}
	if added, err := e.AddSiren("dev_bell", registered("dev_bell", "siren")); !added || err != nil {
		t.Fatalf("%v %v", added, err)
	}
	if removed, err := e.RemoveSiren("dev_siren"); !removed || err != nil {
		t.Fatalf("%v %v", removed, err)
	}
	if removed, err := e.RemoveSiren("dev_nope"); removed || err != nil {
		t.Fatalf("not one: %v %v", removed, err)
	}
	if s, _ := sites.Read(); !slices.Equal(s.SirenIDs, []string{"dev_bell"}) {
		t.Fatalf("%v", s.SirenIDs)
	}
}

// A siren gets a name for the alerts' texts. Without one, they call it "Sirene" when
// it is the only one, and by its rank among several.
func TestSirenNames(t *testing.T) {
	sites := newSites()
	e := NewEditor(sites)
	if s, _ := sites.Read(); s.SirenName("dev_siren") != "Sir\u00e8ne" {
		t.Fatalf("the only siren, unnamed: %q", s.SirenName("dev_siren"))
	}
	if _, err := e.NameSiren("dev_door", "Porte"); !errors.Is(err, ErrNoSiren) || err.Error() != "dev_door is not a siren" {
		t.Fatalf("a zone is not a siren: %v", err)
	}
	for name, want := range map[string]error{" ": ErrEmptyName, strings.Repeat("x", 33): ErrNameTooLong, "A\u202eB": nil} {
		if _, err := e.NameSiren("dev_siren", name); err == nil || (want != nil && !errors.Is(err, want)) || !strings.HasPrefix(err.Error(), "siren name") {
			t.Errorf("name %q: %v", name, err)
		}
	}
	if old, err := e.NameSiren("dev_siren", "Entr\u00e9e"); old != "" || err != nil {
		t.Fatalf("%q %v", old, err)
	}
	writes := sites.writes
	if old, err := e.NameSiren("dev_siren", "Entr\u00e9e"); old != "Entr\u00e9e" || err != nil || sites.writes != writes {
		t.Fatalf("the same name again must write nothing: %q %v", old, err)
	}
	if _, err := e.AddSiren("dev_bell", registered("dev_bell", "siren")); err != nil {
		t.Fatal(err)
	}
	if s, _ := sites.Read(); s.SirenName("dev_siren") != "Entr\u00e9e" || s.SirenName("dev_bell") != "Sir\u00e8ne 2" {
		t.Fatalf("named, and the second of two: %q, %q", s.SirenName("dev_siren"), s.SirenName("dev_bell"))
	}

	// A siren that stops being one forgets its name; the other is alone again.
	if _, err := e.RemoveSiren("dev_siren"); err != nil {
		t.Fatal(err)
	}
	if s, _ := sites.Read(); len(s.SirenNames) != 0 || s.SirenName("dev_bell") != "Sir\u00e8ne" {
		t.Fatalf("%+v", s)
	}
}

// A siren that also has a zone (a smoke detector that sounds) has its zone's name: it
// takes no name of its own, a zone given to a named siren replaces its name, and the
// default names count only the sirens without a zone.
func TestASirenWithAZoneHasItsZonesName(t *testing.T) {
	sites := newSites()
	e := NewEditor(sites)
	smoke := registered("dev_smoke", "smoke", "siren")
	if _, err := e.AddSiren("dev_smoke", smoke); err != nil {
		t.Fatal(err)
	}
	if _, err := e.NameSiren("dev_smoke", "Cuisine haute"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetZone("dev_smoke", Zone{Name: "Cuisine", Kind: "24h_fire"}, smoke); err != nil {
		t.Fatal(err)
	}
	s, _ := sites.Read()
	if len(s.SirenNames) != 0 || s.SirenName("dev_smoke") != "Cuisine" || s.SirenName("dev_siren") != "Sir\u00e8ne" {
		t.Fatalf("%+v: %q, %q", s, s.SirenName("dev_smoke"), s.SirenName("dev_siren"))
	}
	if _, err := e.NameSiren("dev_smoke", "Reserve"); !errors.Is(err, ErrHasZone) ||
		err.Error() != "dev_smoke has a zone: the alerts call it by its zone's name" {
		t.Fatalf("a siren with a zone named: %v", err)
	}
	// Without its zone, it counts again.
	if _, err := e.Unzone("dev_smoke"); err != nil {
		t.Fatal(err)
	}
	if s, _ := sites.Read(); s.SirenName("dev_siren") != "Sir\u00e8ne 1" || s.SirenName("dev_smoke") != "Sir\u00e8ne 2" {
		t.Fatalf("%q, %q", s.SirenName("dev_siren"), s.SirenName("dev_smoke"))
	}
}

func TestARemovalIsCheckedThenFinished(t *testing.T) {
	sites := newSites()
	e := NewEditor(sites)
	if u, err := e.CheckRemoval("dev_motion"); err != nil || u.Used() || sites.checks != 0 {
		t.Fatalf("an unused device: %+v %v, %d check(s)", u, err, sites.checks)
	}
	if _, err := e.CheckRemoval("dev_siren"); !errors.Is(err, ErrLastSiren) || !strings.Contains(err.Error(), "dev_siren cannot be removed yet") {
		t.Fatalf("the last siren: %v", err)
	}
	u, err := e.CheckRemoval("dev_door")
	if err != nil || u.Zone == nil || u.Zone.Name != "Entree" || u.Siren {
		t.Fatalf("%+v %v", u, err)
	}
	if u, err := e.FinishRemoval("dev_door"); err != nil || u.Zone == nil {
		t.Fatalf("%+v %v", u, err)
	}
	if s, _ := sites.Read(); len(s.Zones) != 0 {
		t.Fatalf("%+v", s)
	}
	writes := sites.writes
	if u, err := e.FinishRemoval("dev_door"); err != nil || u.Used() || sites.writes != writes {
		t.Fatalf("nothing more to remove: %+v %v", u, err)
	}
}

// A removal takes seconds between its check and its end: the other siren cannot be
// taken off meanwhile, or the site would end with none once both are done.
func TestARemovalUnderWayHoldsItsDevice(t *testing.T) {
	sites := newSites()
	sites.site.SirenIDs = []string{"dev_siren", "dev_siren2"}
	e := NewEditor(sites)
	if _, err := e.CheckRemoval("dev_siren"); err != nil {
		t.Fatal(err)
	}
	// A second tap, another tab: refused, and the first removal still holds.
	if _, err := e.CheckRemoval("dev_siren"); !errors.Is(err, ErrRemoving) {
		t.Fatalf("the same removal twice: %v", err)
	}
	if _, err := e.RemoveSiren("dev_siren2"); !errors.Is(err, ErrLastSiren) {
		t.Fatalf("the other siren taken off during the removal: %v", err)
	}
	if _, err := e.CheckRemoval("dev_siren2"); !errors.Is(err, ErrLastSiren) {
		t.Fatalf("both removed at once: %v", err)
	}
	if _, err := e.Unzone("dev_door"); err != nil {
		t.Fatalf("a change the removal does not touch: %v", err)
	}
	if u, err := e.FinishRemoval("dev_siren"); err != nil || !u.Siren {
		t.Fatalf("%+v %v", u, err)
	}
	if s, _ := sites.Read(); !slices.Equal(s.SirenIDs, []string{"dev_siren2"}) {
		t.Fatalf("%+v", s)
	}
	// An order that did not go out frees the device.
	if _, err := e.AddSiren("dev_siren3", registered("dev_siren3", "siren")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CheckRemoval("dev_siren3"); err != nil {
		t.Fatal(err)
	}
	e.CancelRemoval("dev_siren3")
	if _, err := e.RemoveSiren("dev_siren2"); err != nil {
		t.Fatalf("a cancelled removal still holds: %v", err)
	}
}

func TestSiteWithoutLeavesTheOriginalAlone(t *testing.T) {
	s := Site{Zones: map[string]Zone{"a": {Name: "A", Kind: "entry"}}, SirenIDs: []string{"a", "b"},
		SirenNames: map[string]string{"a": "Comptoir", "b": "Reserve"}}
	w := s.Without("a")
	if len(w.Zones) != 0 || !slices.Equal(w.SirenIDs, []string{"b"}) || !maps.Equal(w.SirenNames, map[string]string{"b": "Reserve"}) ||
		len(s.Zones) != 1 || len(s.SirenIDs) != 2 || len(s.SirenNames) != 2 {
		t.Fatalf("%+v %+v", s, w)
	}
	if (Site{}).Without("a").Zones == nil {
		t.Fatal("a nil map")
	}
}
