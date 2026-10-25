package hubconfig

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"safehome/internal/install"
)

// copyExample puts a copy of the example file in a temporary directory, with its
// own state folder.
func copyExample(t *testing.T) (path, stateDir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "hub.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir = t.TempDir()
	data = bytes.Replace(data, []byte("/var/lib/safehome"), []byte(stateDir), 1)
	path = filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	return path, stateDir
}

func TestReadSiteReadsTheFileUntilAToolWroteTheDevices(t *testing.T) {
	path, _ := copyExample(t)
	s, err := ReadSite(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Zones) != 7 || s.Zones["dev_door"] != (install.Zone{Name: "Entree", Kind: "entry"}) {
		t.Fatalf("zones: %+v", s.Zones)
	}
	if !reflect.DeepEqual(s.SirenIDs, []string{"dev_siren1"}) {
		t.Fatalf("sirens: %v", s.SirenIDs)
	}
}

func TestWriteSiteKeepsTheDevicesInTheStateFolder(t *testing.T) {
	path, stateDir := copyExample(t)
	original, _ := os.ReadFile(path)
	s, _ := ReadSite(path)
	s.Zones["dev_k7p2"] = install.Zone{Name: "Porte <A> & B", Kind: "instant"}
	delete(s.Zones, "dev_window")
	s.SirenIDs = append(s.SirenIDs, "dev_siren2")
	if err := WriteSite(path, s); err != nil {
		t.Fatal(err)
	}

	if got, err := ReadSite(path); err != nil || !reflect.DeepEqual(got, s) {
		t.Fatalf("read back %+v (%v), wrote %+v", got, err, s)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, original) {
		t.Fatal("the configuration file was touched: it is read-only on a running hub")
	}
	info, err := os.Stat(filepath.Join(stateDir, DevicesFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}

	// The hub runs with them.
	r, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Hub.Engine.Zones["dev_k7p2"]; !ok || len(r.Hub.SirenIDs) != 2 || r.DevicesSum == "" || r.DevicesErr != nil {
		t.Fatalf("zones %v, sirens %v, sum %q, err %v", r.Hub.Engine.Zones, r.Hub.SirenIDs, r.DevicesSum, r.DevicesErr)
	}
	if r.DevicesSum != DevicesSum(stateDir) {
		t.Fatal("the loaded sum differs from the file's")
	}

	// Writing the same again changes nothing; a change changes the sum.
	before := DevicesSum(stateDir)
	if err := WriteSite(path, s); err != nil || DevicesSum(stateDir) != before {
		t.Fatalf("writing the same site changed the file (%v)", err)
	}
	delete(s.Zones, "dev_k7p2")
	if err := WriteSite(path, s); err != nil || DevicesSum(stateDir) == before {
		t.Fatalf("a change kept the same sum (%v)", err)
	}
	if DevicesSum(t.TempDir()) != "" {
		t.Fatal("a folder without devices file has a sum")
	}
}

// The sirens' names stay with them in the state folder, and the hub runs with them.
// A site without names writes the file as before them.
func TestWriteSiteKeepsTheSirensNames(t *testing.T) {
	path, stateDir := copyExample(t)
	s, _ := ReadSite(path)
	if !maps.Equal(s.SirenNames, map[string]string{"dev_siren1": "Reserve"}) {
		t.Fatalf("the file's names: %v", s.SirenNames)
	}
	s.SirenNames = nil
	if err := WriteSite(path, s); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(stateDir, DevicesFile)); bytes.Contains(data, []byte("siren_names")) {
		t.Fatalf("a site without names: %s", data)
	}
	if r, err := Load(path); err != nil || r.Hub.Escalation.DeviceNames["dev_siren1"] != "Sir\u00e8ne" {
		t.Fatalf("the devices file's sirens come without the file's names: %v %v", r.Hub.Escalation.DeviceNames, err)
	}

	s.SirenNames = map[string]string{"dev_siren1": "Entr\u00e9e"}
	if err := WriteSite(path, s); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadSite(path); err != nil || !reflect.DeepEqual(got, s) {
		t.Fatalf("read back %+v (%v), wrote %+v", got, err, s)
	}
	if r, err := Load(path); err != nil || r.Hub.Escalation.DeviceNames["dev_siren1"] != "Entr\u00e9e" {
		t.Fatalf("%v %v", r.Hub.Escalation.DeviceNames, err)
	}
	s.SirenNames = map[string]string{"dev_door": "Porte"}
	if err := WriteSite(path, s); err == nil || !strings.Contains(err.Error(), "not in siren_ids") {
		t.Fatalf("a name for a device that is no siren: %v", err)
	}
}

func TestWriteSiteEmptiesZones(t *testing.T) {
	path, _ := copyExample(t)
	if err := WriteSite(path, install.Site{SirenIDs: []string{"dev_siren1"}}); err != nil {
		t.Fatal(err)
	}
	s, err := ReadSite(path)
	if err != nil || len(s.Zones) != 0 {
		t.Fatalf("zones %v (%v), want none", s.Zones, err)
	}
}

func TestWriteSiteRefusesWhatTheHubWouldRefuse(t *testing.T) {
	path, stateDir := copyExample(t)
	s, _ := ReadSite(path)
	s.Zones["dev_x"] = install.Zone{Name: "X", Kind: "garage"}
	if err := WriteSite(path, s); err == nil {
		t.Fatal("an unknown zone kind was written")
	}
	s.Zones["dev_x"] = install.Zone{Name: "", Kind: "entry"}
	if err := WriteSite(path, s); err == nil {
		t.Fatal("a zone without a name was written")
	}
	if err := CheckSite(path, s); err == nil {
		t.Fatal("CheckSite accepted what WriteSite refuses")
	}
	if err := CheckSite(path, install.Site{Zones: map[string]install.Zone{}}); err == nil || !strings.Contains(err.Error(), "siren") {
		t.Fatalf("a site without siren: %v", err)
	}
	delete(s.Zones, "dev_x")
	if err := CheckSite(path, s); err != nil {
		t.Fatalf("CheckSite refused a good site: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(stateDir, "*")); len(left) > 0 {
		t.Fatalf("a refused change, or a check, wrote %v", left)
	}
}

func TestADamagedDevicesFileNeverStopsTheHub(t *testing.T) {
	for name, content := range map[string]string{
		"broken JSON":    `{"zones": `,
		"unknown key":    `{"zones": {}, "siren_ids": ["dev_siren1"], "sirens": []}`,
		"no siren":       `{"zones": {}, "siren_ids": []}`,
		"unknown kind":   `{"zones": {"dev_door": {"name": "A", "kind": "garage"}}, "siren_ids": ["dev_siren1"]}`,
		"trailing data":  `{"zones": {}, "siren_ids": ["dev_siren1"]} {}`,
		"stray brace":    `{"zones": {}, "siren_ids": ["dev_siren1"]} }`,
		"not an object":  `[]`,
		"empty siren id": `{"zones": {}, "siren_ids": [""]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path, stateDir := copyExample(t)
			if err := os.WriteFile(filepath.Join(stateDir, DevicesFile), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			r, err := Load(path)
			if err != nil {
				t.Fatalf("the hub would not start: %v", err)
			}
			if r.DevicesErr == nil || r.DevicesSum != "" || len(r.Hub.Engine.Zones) != 7 {
				t.Fatalf("err %v, sum %q, zones %d: want the file's own zones and the reason", r.DevicesErr, r.DevicesSum, len(r.Hub.Engine.Zones))
			}
		})
	}
}

func TestWriteSiteRefusesBrokenInput(t *testing.T) {
	for name, in := range map[string]string{
		"not an object": `["a"]`,
		"no state":      `{"site": "tg-lom-0001"}`,
		"broken":        `{"site": `,
	} {
		path := filepath.Join(t.TempDir(), "hub.json")
		if err := os.WriteFile(path, []byte(in), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := WriteSite(path, install.Site{Zones: map[string]install.Zone{}}); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := ReadSite(path); err == nil {
			t.Errorf("%s: ReadSite accepted it", name)
		}
	}
	if err := WriteSite(filepath.Join(t.TempDir(), "missing.json"), install.Site{}); err == nil {
		t.Error("a missing file was accepted")
	}
}
