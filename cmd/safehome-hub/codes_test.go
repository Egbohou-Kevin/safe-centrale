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
	store, err := filestore.Open(state, nil)
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

	// The codes are saved hashed: nothing in the state directory holds a code.
	entries, _ := os.ReadDir(state)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(state, e.Name()))
		if strings.Contains(string(b), "482915") {
			t.Errorf("%s holds a code in clear", e.Name())
		}
	}
}

func TestCodesSetWhileTheHubRunsTakeEffectWithoutARestart(t *testing.T) {
	broker := mqtttest.Start(t)
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
