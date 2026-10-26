package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/adapter/mqtt/mqtttest"
	"safehome/internal/driver/drivertest"
	"safehome/internal/escalation"
	"safehome/internal/hubconfig"
)

const site = "tg-lom-0001"

// writeConfig writes a configuration for a test hub and returns its path.
func writeConfig(t *testing.T, brokerURL, stateDir string, extra string) string {
	t.Helper()
	body := fmt.Sprintf(`{
  "site": %q,
  "broker_url": %q,
  "client_id": "safehome-hub-test-%d",
  "state_dir": %q,
  "log_level": "debug",
  "hub_device_id": "dev_hubpower",
  "siren_ids": ["dev_siren1"],
  "contacts": [{"id": "u_owner", "name": "Owner", "role": "owner", "channels": ["app", "sms"]}],
  "zones": {
    "dev_door": {"name": "Entrance", "kind": "entry"},
    "dev_back": {"name": "Back", "kind": "instant"}
  },
  "access": {"iterations": 10000},
  "heartbeat_every": "1h",
  "boot_window": "300ms"%s
}`, site, brokerURL, time.Now().UnixNano(), stateDir, extra)
	path := filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// cli runs the program's command line and returns its exit code and outputs.
func cli(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestUsageAndVersion(t *testing.T) {
	if code, out, _ := cli(t, "", "version"); code != 0 || !strings.Contains(out, "safehome-hub") {
		t.Fatalf("%d %q", code, out)
	}
	for _, args := range [][]string{{}, {"fly"}, {"run"}, {"check"}, {"codes"}, {"run", "-nonsense"}} {
		if code, _, errb := cli(t, "", args...); code != 2 || errb == "" {
			t.Errorf("%v: exit %d, stderr %q", args, code, errb)
		}
	}
}

func TestCheck(t *testing.T) {
	path := writeConfig(t, "mqtt://127.0.0.1:1883", t.TempDir(), "")
	code, out, errb := cli(t, "", "check", "-config", path)
	if code != 0 || !strings.Contains(out, "configuration OK") || !strings.Contains(out, "2 zone(s)") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"site": "tg-lom-0001", "exit_dealy": "2m"}`), 0o600)
	if code, _, errb := cli(t, "", "check", "-config", bad); code != 1 || !strings.Contains(errb, "configuration error") {
		t.Fatalf("%d %q", code, errb)
	}
	if code, _, _ := cli(t, "", "check", "-config", filepath.Join(t.TempDir(), "missing.json")); code != 1 {
		t.Fatalf("a missing file: exit %d", code)
	}
}

// A site without a GSM line says so at the check, and names a contact nobody would
// tell anything.
func TestTheCheckSaysASiteHasNoGSMLine(t *testing.T) {
	path := writeConfig(t, "mqtt://127.0.0.1:1883", t.TempDir(), `, "without_gsm": true`)
	code, out, errb := cli(t, "", "check", "-config", path)
	if code != 0 || !strings.Contains(out, "no GSM line") || strings.Contains(out, "accepts only SMS") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	var b bytes.Buffer
	withoutGSM([]escalation.Contact{
		{ID: "u_owner", Channels: []escalation.Channel{escalation.ChannelApp, escalation.ChannelSMS}},
		{ID: "u_guard", Channels: []escalation.Channel{escalation.ChannelSMS, escalation.ChannelCall}},
	}, &b)
	if !strings.Contains(b.String(), "contact u_guard accepts only SMS and calls") || strings.Contains(b.String(), "u_owner") {
		t.Fatalf("%q", b.String())
	}
}

// Zones set by the installer's tools that the hub would leave aside stop the
// installation script: the hub would watch the configuration file's older ones.
func TestTheCheckFailsWhenTheDevicesFileIsLeftAside(t *testing.T) {
	state := t.TempDir()
	path := writeConfig(t, "mqtt://127.0.0.1:1883", state, "")
	devices := `{"zones": {"dev_door": {"name": "Entrance", "kind": "garage"}}, "siren_ids": []}`
	if err := os.WriteFile(filepath.Join(state, hubconfig.DevicesFile), []byte(devices), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errb := cli(t, "", "check", "-config", path)
	if code != 1 || !strings.Contains(errb, "configuration error") || !strings.Contains(errb, hubconfig.DevicesFile) {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
	}
}

// A site without a siren, by choice (26 October 2026), passes the check, which says it:
// the installation script goes on.
func TestTheCheckAcceptsASiteWithoutASiren(t *testing.T) {
	state := t.TempDir()
	path := writeConfig(t, "mqtt://127.0.0.1:1883", state, "")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `["dev_siren1"]`, `[]`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errb := cli(t, "", "check", "-config", path)
	if code != 0 || !strings.Contains(out, "0 siren(s)") || !strings.Contains(out, "no siren, by choice") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
	}
}

// fakeSystemd listens where systemd would, and collects what the program says.
func fakeSystemd(t *testing.T) func() []string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sdn") // a short path: Unix sockets have a small limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	t.Setenv("WATCHDOG_USEC", "300000") // 300 ms
	var mu sync.Mutex
	var got []string
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			mu.Lock()
			got = append(got, string(buf[:n]))
			mu.Unlock()
		}
	}()
	return func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }
}

func TestTheHubTellsSystemdItIsReadyAndStillWorks(t *testing.T) {
	said := fakeSystemd(t)
	broker := mqtttest.Start(t)
	state := t.TempDir()
	l := startLive(t, broker, state, writeConfig(t, broker.URL(), state, ""))
	l.waitState("disarmed")
	pings := func() int { return strings.Count(strings.Join(said(), "\n"), "WATCHDOG=1") }
	drivertest.WaitFor(t, "ready, then pings", func() bool { return len(said()) > 0 && said()[0] == "READY=1" && pings() >= 3 })
	if code := l.stopHub(); code != 0 {
		t.Fatal(code)
	}
	if !slices.Contains(said(), "STOPPING=1") {
		t.Fatalf("%q", said())
	}
}
