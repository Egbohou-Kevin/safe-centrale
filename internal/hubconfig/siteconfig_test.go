package hubconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
	"safehome/internal/siteconfig"
)

// exampleWithState copies the example configuration with its state in a temporary
// folder, and returns the file and the folder.
func exampleWithState(t *testing.T) (string, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "hub.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	data = []byte(strings.Replace(string(data), `"/var/lib/safehome"`, `"`+state+`"`, 1))
	path := filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, state
}

func cloudConfig() contract.ConfigSetData {
	return contract.ConfigSetData{
		Version: 4, SiteName: "Boutique Be",
		Contacts: []contract.ConfigContact{
			{ID: "u_owner", Name: "Ama", Role: "owner", Channels: []string{"app", "sms"}, Phone: "+22890000001"},
			{ID: "u_night", Name: "Kofi", Role: "guard", Channels: []string{"sms", "call"}, Phone: "+22870000002"},
		},
		Closing:    map[string]string{"daily": "19:30", "sun": "off"},
		ExitDelayS: 60, EntryDelayS: 30,
	}
}

func TestTheCloudConfigurationIsLaidOverTheFile(t *testing.T) {
	path, state := exampleWithState(t)
	base, err := Load(path)
	if err != nil || base.SiteVersion != 0 || base.SiteErr != nil {
		t.Fatalf("%v %d %v", err, base.SiteVersion, base.SiteErr)
	}
	if err := siteconfig.Write(state, cloudConfig()); err != nil {
		t.Fatal(err)
	}
	r, err := Load(path)
	if err != nil || r.SiteErr != nil || r.SiteVersion != 4 {
		t.Fatalf("%v %v %d", err, r.SiteErr, r.SiteVersion)
	}
	e := r.Hub.Escalation
	if e.SiteName != "Boutique Be" || len(e.Contacts) != 2 || e.Contacts[1].ID != "u_night" || e.Contacts[1].Role != "guard" {
		t.Fatalf("%+v", e.Contacts)
	}
	eng := r.Hub.Engine
	if eng.ExitDelay != time.Minute || eng.EntryDelay != 30*time.Second {
		t.Fatalf("%v %v", eng.ExitDelay, eng.EntryDelay)
	}
	if (eng.Closing[time.Monday] != engine.TimeOfDay{Hour: 19, Minute: 30}) || len(eng.Closing) != 6 {
		t.Fatalf("%v", eng.Closing)
	}
	// What the cloud does not set stays as the file says.
	if eng.SirenCycle != base.Hub.Engine.SirenCycle || len(eng.Zones) != len(base.Hub.Engine.Zones) || r.StateDir != state {
		t.Fatal("the rest of the file must not change")
	}

	// Only the version: everything from the file.
	siteconfig.Write(state, contract.ConfigSetData{Version: 5})
	r, _ = Load(path)
	if r.SiteVersion != 5 || len(r.Hub.Escalation.Contacts) != len(base.Hub.Escalation.Contacts) || r.Hub.Engine.EntryDelay != base.Hub.Engine.EntryDelay {
		t.Fatalf("%+v", r)
	}
}

func TestACloudConfigurationThatDoesNotFitNeverStopsTheHub(t *testing.T) {
	path, state := exampleWithState(t)
	base, _ := Load(path)

	// No owner: the escalation refuses it.
	noOwner := cloudConfig()
	noOwner.Contacts = noOwner.Contacts[1:]
	if _, err := WithSiteConfig(path, noOwner); err == nil {
		t.Fatal("a configuration without owner was accepted")
	}
	siteconfig.Write(state, noOwner)
	r, err := Load(path)
	if err != nil || r.SiteErr == nil || r.SiteVersion != 4 {
		t.Fatalf("%v %v %d", err, r.SiteErr, r.SiteVersion)
	}
	if len(r.Hub.Escalation.Contacts) != len(base.Hub.Escalation.Contacts) {
		t.Fatal("the hub runs on its own file")
	}

	// A file a stricter contract refuses (written before an update): refused under its
	// version, which the hub tells the cloud.
	os.WriteFile(filepath.Join(state, siteconfig.FileName), []byte(`{"version":7,"entry_delay_s":900}`), 0o600)
	if r, err := Load(path); err != nil || r.SiteErr == nil || r.SiteVersion != 7 {
		t.Fatalf("%v %v %d", err, r.SiteErr, r.SiteVersion)
	}
	// A damaged file too.
	os.WriteFile(filepath.Join(state, siteconfig.FileName), []byte("{"), 0o600)
	if r, err := Load(path); err != nil || r.SiteErr == nil || r.SiteVersion != 0 {
		t.Fatalf("%v %v", err, r.SiteErr)
	}
	// A bad closing time, which only the hub knows how to read.
	bad := cloudConfig()
	bad.Closing = map[string]string{"someday": "20:00"}
	if _, err := WithSiteConfig(path, bad); err == nil || !strings.Contains(err.Error(), "someday") {
		t.Fatalf("%v", err)
	}
	if _, err := WithSiteConfig(filepath.Join(t.TempDir(), "none.json"), cloudConfig()); err == nil {
		t.Fatal("a missing file")
	}
}

// The check of a configuration from the cloud sees the devices as the hub loads them:
// a devices file the hub leaves aside must not make every configuration refused.
func TestTheCloudCheckLaysTheDevicesAsTheHubDoes(t *testing.T) {
	path, state := exampleWithState(t)
	if err := os.WriteFile(filepath.Join(state, DevicesFile), []byte(`{"zones": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := Load(path); err != nil || r.DevicesErr == nil {
		t.Fatalf("the devices file must be left aside: %v %v", err, r.DevicesErr)
	}
	r, err := WithSiteConfig(path, cloudConfig())
	if err != nil {
		t.Fatalf("refused because of devices the hub does not run with: %v", err)
	}
	if len(r.Hub.SirenIDs) != 1 || len(r.Hub.Engine.Zones) != 7 {
		t.Fatalf("sirens %v, %d zones: want the file's own", r.Hub.SirenIDs, len(r.Hub.Engine.Zones))
	}
}

// A room the hub clears costs only itself with a configuration from the cloud too, the
// normal case on a real site: the cloud check does not refuse that configuration, and
// once it is stored the hub keeps every zone and still says which room it cleared.
func TestABadRoomCostsOnlyItselfUnderTheCloudConfiguration(t *testing.T) {
	path, state := exampleWithState(t)
	devices := `{"zones": {
		"dev_door": {"name": "Entrance", "kind": "entry", "room": "` + strings.Repeat("x", 40) + `"},
		"dev_back": {"name": "Back", "kind": "instant"},
		"dev_pir": {"name": "Living room", "kind": "interior", "room": "Salon"},
		"dev_smoke": {"name": "Kitchen", "kind": "24h_fire"}
	}, "siren_ids": ["dev_siren1"]}`
	if err := os.WriteFile(filepath.Join(state, DevicesFile), []byte(devices), 0o600); err != nil {
		t.Fatal(err)
	}
	checked, err := WithSiteConfig(path, cloudConfig())
	if err != nil {
		t.Fatalf("the cloud configuration was refused for a room: %v", err)
	}
	if z := checked.Hub.Engine.Zones; len(z) != 4 || z["dev_door"].Name != "Entrance" || z["dev_door"].Room != "" {
		t.Fatalf("the cloud check saw %+v, want the file's 4 zones, dev_door without a room", z)
	}
	if err := siteconfig.Write(state, cloudConfig()); err != nil {
		t.Fatal(err)
	}
	r, err := Load(path)
	if err != nil || r.SiteErr != nil || r.SiteVersion != 4 || r.DevicesErr != nil || r.DevicesSum == "" {
		t.Fatalf("%v, site %v (version %d), devices %v", err, r.SiteErr, r.SiteVersion, r.DevicesErr)
	}
	zones := r.Hub.Engine.Zones
	if len(zones) != 4 || zones["dev_door"].Room != "" || zones["dev_pir"].Room != "Salon" {
		t.Fatalf("%d zone(s) watched: %+v", len(zones), zones)
	}
	want := []RoomLeftAside{{Zone: "dev_door", Reason: "it is longer than 32 characters"}}
	if !reflect.DeepEqual(r.RoomsLeftAside, want) {
		t.Fatalf("rooms left aside %+v, want %+v", r.RoomsLeftAside, want)
	}
}

// An empty list from the cloud (no automatic arming) is a list sent: it is checked as
// such, and the hub runs on it after the restart too, not on its file's. An empty list
// of contacts is refused: every alarm would go unheard.
func TestEmptyListsFromTheCloudOutliveTheRestart(t *testing.T) {
	path, state := exampleWithState(t)
	if _, err := WithSiteConfig(path, contract.ConfigSetData{Version: 7, Contacts: []contract.ConfigContact{}}); err == nil {
		t.Fatal("a configuration without any contact was taken")
	}
	owner := []contract.ConfigContact{{ID: "u_owner", Name: "Owner", Role: "owner", Channels: []string{"app"}}}
	d := contract.ConfigSetData{Version: 7, Contacts: owner, Closing: map[string]string{}}
	checked, err := WithSiteConfig(path, d)
	if err != nil || len(checked.Hub.Escalation.Contacts) != 1 || len(checked.Hub.Engine.Closing) != 0 {
		t.Fatalf("checked: %v, %d contact(s), %d closing day(s)", err, len(checked.Hub.Escalation.Contacts), len(checked.Hub.Engine.Closing))
	}
	if err := siteconfig.Write(state, d); err != nil {
		t.Fatal(err)
	}
	r, err := Load(path)
	if err != nil || r.SiteErr != nil || r.SiteVersion != 7 {
		t.Fatalf("%v %v %d", err, r.SiteErr, r.SiteVersion)
	}
	if len(r.Hub.Escalation.Contacts) != 1 || len(r.Hub.Engine.Closing) != 0 {
		t.Fatalf("after the restart: %d contact(s), %d closing day(s), the file's lists came back", len(r.Hub.Escalation.Contacts), len(r.Hub.Engine.Closing))
	}
}
