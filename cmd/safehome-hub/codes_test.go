package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"safehome/internal/adapter/filestore"
	"safehome/internal/adapter/mqtt/mqtttest"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/driver/drivertest"
	"safehome/internal/escalation"
	"safehome/internal/hub"
	"safehome/internal/hubconfig"
)

// moreContacts gives the site of the configuration at path a deputy and a guard too.
func moreContacts(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	owner := `{"id": "u_owner", "name": "Owner", "role": "owner", "channels": ["app", "sms"]}`
	more := owner + `, {"id": "u_deputy", "name": "Deputy", "role": "deputy", "channels": ["app"]}, {"id": "u_guard", "name": "Guard", "role": "guard", "channels": ["app"]}`
	if err := os.WriteFile(path, bytes.Replace(data, []byte(owner), []byte(more), 1), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodesCommands(t *testing.T) {
	state := t.TempDir()
	path := writeConfig(t, "mqtt://127.0.0.1:1883", state, "")
	moreContacts(t, path)

	if code, out, _ := cli(t, "", "codes", "-config", path, "list"); code != 0 || !strings.Contains(out, "no access code") {
		t.Fatalf("%d %q", code, out)
	}
	code, out, errb := cli(t, "482915\n", "codes", "-config", path, "set", "u_owner")
	if code != 0 || !strings.Contains(out, "code set for u_owner") || !strings.Contains(out, "no hub is running") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	if strings.Contains(out+errb, "482915") {
		t.Fatal("the code must never be printed")
	}
	cli(t, "730264\n", "codes", "-config", path, "set", "u_guard")
	if _, out, _ := cli(t, "", "codes", "-config", path, "list"); out != "u_owner\nu_guard\n" {
		t.Fatalf("%q", out)
	}

	// What is refused, and why, without ever echoing the code.
	for name, c := range map[string]struct {
		stdin string
		args  []string
		want  string
	}{
		"weak code":     {"1234\n", []string{"set", "u_deputy"}, "too easy to guess"},
		"not digits":    {"48a915\n", []string{"set", "u_deputy"}, "4 to 8 digits"},
		"taken":         {"482915\n", []string{"set", "u_deputy"}, "already has this code"},
		"no code":       {"", []string{"set", "u_deputy"}, "no code given"},
		"blank code":    {"   \n", []string{"set", "u_deputy"}, "no code given"},
		"not a contact": {"615273\n", []string{"set", "u_x"}, "u_x is not a contact of the site"},
		"unknown user":  {"", []string{"remove", "nobody"}, "no such user"},
	} {
		code, out, errb := cli(t, c.stdin, append([]string{"codes", "-config", path}, c.args...)...)
		if code != 1 || !strings.Contains(errb, c.want) {
			t.Errorf("%s: exit %d stderr %q", name, code, errb)
		}
		if strings.Contains(out+errb, "48a915") || strings.Contains(out+errb, "482915") {
			t.Errorf("%s: the code is echoed back: %q %q", name, out, errb)
		}
	}
	if code, _, _ := cli(t, "", "codes", "-config", path, "set"); code != 2 {
		t.Errorf("set without a user: exit %d", code)
	}
	if code, _, _ := cli(t, "", "codes", "-config", path, "remove"); code != 2 {
		t.Errorf("remove without a user: exit %d", code)
	}
	if code, _, _ := cli(t, "", "codes", "-config", path, "explode"); code != 2 {
		t.Errorf("unknown subcommand: exit %d", code)
	}

	if code, out, _ := cli(t, "", "codes", "-config", path, "remove", "u_guard"); code != 0 || !strings.Contains(out, "code removed for u_guard") {
		t.Fatalf("%d %q", code, out)
	}
	if _, out, _ := cli(t, "", "codes", "-config", path, "list"); out != "u_owner\n" {
		t.Fatalf("%q", out)
	}

	// The installer code: apart from the people's, and never the same as one of them.
	if code, _, errb := cli(t, "482915\n", "codes", "-config", path, "set", "installer"); code != 1 || !strings.Contains(errb, "already has this code") {
		t.Fatalf("an installer code equal to the owner's: %d %q", code, errb)
	}
	if code, out, errb := cli(t, "259031\n", "codes", "-config", path, "set", "installer"); code != 0 || !strings.Contains(out, "code set for installer") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	if code, _, errb := cli(t, "259031\n", "codes", "-config", path, "set", "u_guard"); code != 1 || !strings.Contains(errb, "already has this code") {
		t.Fatalf("a person's code equal to the installer's: %d %q", code, errb)
	}
	if _, out, _ := cli(t, "", "codes", "-config", path, "list"); out != "u_owner\ninstaller\n" {
		t.Fatalf("%q", out)
	}

	// A code left by someone no longer a contact (the cloud's configuration changed the
	// contacts) is listed as such, and can be removed.
	gone, err := hubconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := filestore.Open(hubconfig.CodesDir(state), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Three codes were refused as taken above: the tools now wait 10 minutes, which
	// this test does not.
	if err := store.Save("codes-taken", []byte("[]")); err != nil {
		t.Fatal(err)
	}
	before := gone.Hub
	before.Escalation.Contacts = append(slices.Clone(before.Escalation.Contacts), escalation.Contact{ID: "u_gone"})
	if tool, err := hub.OpenSiteCodes(before, store, driver.SystemClock()); err != nil || tool.Set("u_gone", "615273") != nil {
		t.Fatal(err)
	}
	if _, out, _ := cli(t, "", "codes", "-config", path, "list"); !strings.Contains(out, "u_gone (not a contact of the site") {
		t.Fatalf("%q", out)
	}
	if code, out, _ := cli(t, "", "codes", "-config", path, "remove", "u_gone"); code != 0 || !strings.Contains(out, "code removed for u_gone") {
		t.Fatalf("%d %q", code, out)
	}

	// The codes are saved hashed: nothing in the state directory holds a code, nor in
	// the codes' folder; a code in clear slipped in on purpose shows.
	read, clear := inClear(t, state, "482915")
	if len(clear) != 0 || !slices.Contains(read, filepath.Join(hubconfig.CodesFolder, "access.json")) {
		t.Fatalf("read %v, holding a code in clear %v", read, clear)
	}
	// The tool writes them in their folder, under the lock in it.
	for _, name := range []string{"access.json", "installer.json", "codes-taken.json", hubconfig.CodesLock} {
		if !slices.Contains(read, filepath.Join(hubconfig.CodesFolder, name)) || slices.Contains(read, name) {
			t.Errorf("%s is not in the codes' folder alone: %v", name, read)
		}
	}
	planted := filepath.Join(hubconfig.CodesDir(state), "planted.json")
	if err := os.WriteFile(planted, []byte(`["482915"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, clear := inClear(t, state, "482915"); !slices.Equal(clear, []string{filepath.Join(hubconfig.CodesFolder, "planted.json")}) {
		t.Fatalf("the code slipped in is not seen: %v", clear)
	}
}

// inClear reads every file of the state folder dir and of its folders, and returns
// those read, and those holding code, by their path in dir.
func inClear(t *testing.T, dir, code string) (read, clear []string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		read = append(read, rel)
		if strings.Contains(string(b), code) {
			clear = append(clear, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return read, clear
}

// The hub starts with the codes wherever they are: still in the state folder, where
// they were kept before (install.sh moves them), or in their own folder, which wins
// when both hold them. A hub started without its codes could not be disarmed.
func TestTheHubStartsWithItsCodesWhereverTheyAre(t *testing.T) {
	for name, c := range map[string]struct {
		flat, folder string // u_owner's code kept there, "" for none
		right, wrong string
	}{
		"in the state folder only": {flat: "482915", right: "482915", wrong: "111122"},
		"in their folder only":     {folder: "730264", right: "730264", wrong: "111122"},
		"in both":                  {flat: "482915", folder: "730264", right: "730264", wrong: "482915"},
	} {
		t.Run(name, func(t *testing.T) {
			broker := mqtttest.Start(t, "safehome-hub")
			state := t.TempDir()
			cfg := writeConfig(t, broker.URL(), state, "")
			loaded, err := hubconfig.Load(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for dir, code := range map[string]string{state: c.flat, hubconfig.CodesDir(state): c.folder} {
				if code == "" {
					continue
				}
				store, err := filestore.Open(dir, nil)
				if err != nil {
					t.Fatal(err)
				}
				site, err := hub.OpenSiteCodes(loaded.Hub, store, driver.SystemClock())
				if err != nil || site.Set("u_owner", code) != nil {
					t.Fatal(err)
				}
			}
			l := startLive(t, broker, state, cfg)
			l.waitState("disarmed")
			l.event("dev_keypad", contract.CapKeypad, "arm", "")
			l.waitState("arming")
			l.event("dev_keypad", contract.CapKeypad, "arm_now", "")
			l.waitState("armed")
			l.event("dev_keypad", contract.CapKeypad, "disarm", c.wrong)
			l.waitCount(contract.TypeAlarmAuthFailed, 1)
			l.event("dev_keypad", contract.CapKeypad, "disarm", c.right)
			l.waitState("disarmed")
			if n := len(l.seen(contract.TypeAlarmAuthFailed)); n != 1 {
				t.Fatalf("%d codes refused", n)
			}
		})
	}
}

func TestCodesSetWhileTheHubRunsTakeEffectWithoutARestart(t *testing.T) {
	broker := mqtttest.Start(t, "safehome-hub")
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state, "")
	l := startLive(t, broker, state, cfg)
	l.waitState("disarmed")
	l.event("dev_keypad", contract.CapKeypad, "arm", "")
	l.waitState("arming")
	l.event("dev_keypad", contract.CapKeypad, "arm_now", "")
	l.waitState("armed")

	// The installer sets a code with the command-line tool; the running hub is told.
	code, out, errb := cli(t, "730264\n", "codes", "-config", cfg, "set", "u_owner")
	if code != 0 || !strings.Contains(out, "told to reload") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	drivertest.WaitFor(t, "the reload", func() bool { return strings.Contains(l.logs.String(), "access codes reloaded") })
	l.event("dev_keypad", contract.CapKeypad, "disarm", "730264")
	l.waitState("disarmed")
	if d, _ := contract.DecodeData[contract.DisarmedData](l.seen(contract.TypeAlarmDisarmed)[0]); d.User != "u_owner" {
		t.Fatalf("%+v", d)
	}
	l.validateEverything("730264")
}
