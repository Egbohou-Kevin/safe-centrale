package siteconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"safehome/internal/contract"
)

func TestWriteThenRead(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := Read(dir); ok || err != nil {
		t.Fatalf("nothing set yet: %v %v", ok, err)
	}
	q := 500
	d := contract.ConfigSetData{
		Version: 3, SiteName: "Boutique",
		Contacts: []contract.ConfigContact{{ID: "u_owner", Name: "P", Role: "owner", Channels: []string{"sms"}, Phone: "+22890000001"}},
		Closing:  map[string]string{"daily": "20:00"}, EntryDelayS: 30, Video: &contract.ConfigVideo{QuotaMB: &q},
	}
	if err := Write(dir, d); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Read(dir)
	if err != nil || !ok || !reflect.DeepEqual(got, d) {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	for _, path := range []string{filepath.Join(dir, Dir, FileName), filepath.Join(dir, FileName)} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s holds phone numbers: %v %v", path, fi, err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, Dir)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("%s/: %v", Dir, fi.Mode())
	}
	d.Version = 4
	if err := Write(dir, d); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := Read(dir); got.Version != 4 {
		t.Fatal("replaced")
	}
	// The video service sees Dir alone: the latest version is there.
	if got, _, _ := readFile(filepath.Join(dir, Dir, FileName)); got.Version != 4 {
		t.Fatalf("%s/ holds version %d", Dir, got.Version)
	}
	for _, d := range []string{dir, filepath.Join(dir, Dir)} {
		if entries, _ := os.ReadDir(d); len(entries) != 2 && d == dir || len(entries) != 1 && d != dir {
			t.Fatalf("no temporary file left in %s: %v", d, entries)
		}
	}
}

// A program put back by a rollback writes the top copy alone; a hub updated before its
// first configuration holds no other; the video service sees Dir's alone.
func TestTheHigherCopyWins(t *testing.T) {
	put := func(t *testing.T, path string, version int) {
		t.Helper()
		os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, fmt.Appendf(nil, `{"version":%d,"entry_delay_s":30}`, version), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name         string
		inDir, atTop int // 0: no file
		want         int
	}{
		{"rolled back, then updated", 3, 4, 4},
		{"written by this version", 4, 4, 4},
		{"top one left from before", 5, 4, 5},
		{"updated, no configuration since", 0, 4, 4},
		{"the video's view", 4, 0, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.inDir > 0 {
				put(t, filepath.Join(dir, Dir, FileName), c.inDir)
			}
			if c.atTop > 0 {
				put(t, filepath.Join(dir, FileName), c.atTop)
			}
			if got, found, err := Read(dir); err != nil || !found || got.Version != c.want {
				t.Fatalf("version %d %v %v, want %d", got.Version, found, err, c.want)
			}
		})
	}
	dir := t.TempDir()
	put(t, filepath.Join(dir, FileName), 4)
	os.Mkdir(filepath.Join(dir, Dir), 0o700)
	os.WriteFile(filepath.Join(dir, Dir, FileName), []byte("{"), 0o600)
	if got, _, err := Read(dir); err != nil || got.Version != 4 {
		t.Fatalf("a damaged copy in %s/ hid the top one: %d %v", Dir, got.Version, err)
	}
}

func TestBadConfigurationsAreRefused(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, contract.ConfigSetData{}); err == nil {
		t.Fatal("version 0 written")
	}
	os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o600)
	if _, _, err := Read(dir); err == nil {
		t.Fatal("a damaged file read")
	}
	// A file that reads but no longer fits: refused, under its version, for the hub
	// to tell the cloud which version it refuses.
	os.WriteFile(filepath.Join(dir, FileName), []byte(`{"version":2,"entry_delay_s":900}`), 0o600)
	if got, found, err := Read(dir); err == nil || !found || !reflect.DeepEqual(got, contract.ConfigSetData{Version: 2}) {
		t.Fatalf("an invalid file: %+v %v %v", got, found, err)
	}
	if err := Write(filepath.Join(dir, "none"), contract.ConfigSetData{Version: 1}); err == nil {
		t.Fatal("written in a missing folder")
	}
}

func appliedMsg(t *testing.T, site string, version int, status string) []byte {
	t.Helper()
	env, err := contract.NewEnvelope(site, "config", 1, contract.NewIDGenerator().New(), time.Now(), contract.TypeConfigApplied,
		contract.ConfigAppliedData{Version: version, Status: status})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	return b
}

func TestChanged(t *testing.T) {
	const site = "tg-lom-0001"
	dir := t.TempDir()
	if _, ok := Changed(dir, site, 0, appliedMsg(t, site, 3, contract.ConfigApplied)); ok {
		t.Fatal("no file: nothing to load")
	}
	Write(dir, contract.ConfigSetData{Version: 3})
	if v, ok := Changed(dir, site, 0, appliedMsg(t, site, 3, contract.ConfigApplied)); !ok || v != 3 {
		t.Fatalf("%d %v", v, ok)
	}
	for name, c := range map[string]struct {
		running int
		msg     []byte
	}{
		"running it":       {3, appliedMsg(t, site, 3, contract.ConfigApplied)},
		"rejected":         {0, appliedMsg(t, site, 3, contract.ConfigRejected)},
		"another site":     {0, appliedMsg(t, "tg-lom-0002", 3, contract.ConfigApplied)},
		"garbage":          {0, []byte("{")},
		"the file decides": {3, appliedMsg(t, site, 2, contract.ConfigApplied)}, // retained, older
	} {
		if _, ok := Changed(dir, site, c.running, c.msg); ok {
			t.Errorf("%s: restart", name)
		}
	}
	os.WriteFile(filepath.Join(dir, Dir, FileName), []byte("{"), 0o600)
	os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o600)
	if _, ok := Changed(dir, site, 0, appliedMsg(t, site, 3, contract.ConfigApplied)); ok {
		t.Fatal("a damaged file would restart the program forever")
	}
}

// An empty list is kept as sent, apart from a list not sent: "closing": {} removes
// every automatic arming, no "closing" leaves the program's file in charge. An empty
// list of contacts is refused: every alarm would go unheard.
func TestAnEmptyListIsKept(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, contract.ConfigSetData{Version: 2, Contacts: []contract.ConfigContact{}}); err == nil {
		t.Fatal("a configuration without any contact was written")
	}
	d := contract.ConfigSetData{Version: 2, Closing: map[string]string{}}
	if err := Write(dir, d); err != nil {
		t.Fatal(err)
	}
	if got, _, err := Read(dir); err != nil || got.Closing == nil {
		t.Fatalf("%+v %v: the empty list was dropped", got, err)
	}
	if err := Write(dir, contract.ConfigSetData{Version: 3}); err != nil {
		t.Fatal(err)
	}
	if got, _, err := Read(dir); err != nil || got.Contacts != nil || got.Closing != nil {
		t.Fatalf("%+v %v: lists not sent came back as sent", got, err)
	}
}
