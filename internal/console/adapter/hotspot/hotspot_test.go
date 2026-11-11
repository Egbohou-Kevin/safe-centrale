package hotspot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeNMCLI writes a script that logs its arguments, one call per line, and fails
// for the subcommands listed in $FAIL ("connection up", ...).
func fakeNMCLI(t *testing.T) (path, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	path = filepath.Join(dir, "nmcli")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$FAIL\" in *\"$1 $2\"*) echo \"Error: no Wi-Fi device; psk $*\" >&2; exit 4;; esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, log
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// good is a network whose interface forwards nothing, as the dispatcher script leaves
// it once the network is up.
func good(t *testing.T, nmcli string) Config {
	return Config{Interface: "wlan0", SSID: "safehome-tg-lom-0001", Passphrase: "k7p2m9x4q8", Address: "10.42.0.1", NMCLI: nmcli,
		Forwarding: forwarding(t, "0")}
}

// forwarding makes a folder of IPv4 settings where wlan0's forwarding is value.
func forwarding(t *testing.T, value string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "wlan0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wlan0", "forwarding"), []byte(value+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestUpMakesAnAccessPointThatNeverStartsAtBoot(t *testing.T) {
	nmcli, log := fakeNMCLI(t)
	h, err := New(good(t, nmcli))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := calls(t, log)
	if len(got) != 3 || got[0] != "connection delete safehome-console" || got[2] != "connection up safehome-console" {
		t.Fatalf("calls %q", got)
	}
	for _, want := range []string{"ifname wlan0", "autoconnect no", "ssid safehome-tg-lom-0001", "802-11-wireless.mode ap",
		"ipv4.method shared", "ipv4.addresses 10.42.0.1/24", "wifi-sec.key-mgmt wpa-psk", "wifi-sec.psk k7p2m9x4q8"} {
		if !strings.Contains(got[1], want) {
			t.Errorf("connection add lacks %q: %s", want, got[1])
		}
	}

	if err := h.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	got = calls(t, log)
	if got[3] != "connection down safehome-console" || got[4] != "connection delete safehome-console" {
		t.Fatalf("down: %q", got[3:])
	}
}

func TestAFailedStartLeavesNoNetworkAndNoPassphraseInTheError(t *testing.T) {
	nmcli, log := fakeNMCLI(t)
	t.Setenv("FAIL", "connection up")
	h, _ := New(good(t, nmcli))
	err := h.Up(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no Wi-Fi device") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), "k7p2m9x4q8") {
		t.Fatalf("the passphrase is in the error: %v", err)
	}
	if got := calls(t, log); got[len(got)-1] != "connection delete safehome-console" {
		t.Fatalf("the half-made network is left: %q", got)
	}
}

func TestDownRunsEvenAfterTheEnd(t *testing.T) {
	nmcli, log := fakeNMCLI(t)
	h, _ := New(good(t, nmcli))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if got := calls(t, log); len(got) != 2 {
		t.Fatalf("calls %q", got)
	}
}

func TestNewRefusesABadNetwork(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"no interface":     func(c *Config) { c.Interface = "" },
		"no name":          func(c *Config) { c.SSID = "" },
		"long name":        func(c *Config) { c.SSID = strings.Repeat("x", 33) },
		"short passphrase": func(c *Config) { c.Passphrase = "1234567" },
		"long passphrase":  func(c *Config) { c.Passphrase = strings.Repeat("x", 64) },
		"accented":         func(c *Config) { c.Passphrase = "\u00e9t\u00e9-\u00e0-lom\u00e9" },
		"control":          func(c *Config) { c.Passphrase = "k7p2m9x4\tq8" },
		"no address":       func(c *Config) { c.Address = "" },
		"IPv6 address":     func(c *Config) { c.Address = "fe80::1" },
		"path":             func(c *Config) { c.Interface = "../wlan0" },
	} {
		c := good(t, "nmcli")
		edit(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A hung nmcli must not block the stop: Down keeps the caller's deadline, even though
// it ignores its cancellation.
func TestDownKeepsTheCallersDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nmcli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h, _ := New(good(t, path))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := h.Down(ctx); err == nil {
		t.Fatal("a hung nmcli was taken for a success")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("Down took %v past its deadline", took)
	}
}

// overdue is a context whose deadline is over, ended by its cancel: a start cut short
// by its deadline, without waiting for one.
type overdue struct{ context.Context }

func (overdue) Deadline() (time.Time, bool) { return time.Now().Add(-time.Second), true }

// A start cut short by its deadline: NetworkManager may still bring the network up
// after nmcli is killed, so it is deleted all the same, on a time of its own (the
// caller's is over). The caller's context ends while "connection up" hangs.
func TestAStartPastItsDeadlineIsCleanedUp(t *testing.T) {
	dir := t.TempDir()
	log, path := filepath.Join(dir, "log"), filepath.Join(dir, "nmcli")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$1 $2\" in \"connection up\") exec sleep 5;; esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h, _ := New(good(t, path))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		for ctx.Err() == nil && !strings.Contains(strings.Join(calls(t, log), "\n"), "connection up") {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	if err := h.Up(overdue{ctx}); err == nil {
		t.Fatal("a start past its deadline was taken for a success")
	}
	if got := calls(t, log); len(got) != 4 || got[3] != "connection delete safehome-console" {
		t.Fatalf("the network may come up and stay: %q", got)
	}
}

// NetworkManager's shared mode routes the phones to the hub's other networks: a network
// whose interface still forwards once up is taken down, not left for the installation.
func TestARoutedNetworkIsTakenDown(t *testing.T) {
	for name, dir := range map[string]string{
		"forwarding on": forwarding(t, "1"),
		"no setting":    t.TempDir(),
	} {
		t.Run(name, func(t *testing.T) {
			nmcli, log := fakeNMCLI(t)
			c := good(t, nmcli)
			c.Forwarding, c.RoutingWait = dir, 300*time.Millisecond
			h, _ := New(c)
			if err := h.Up(context.Background()); err == nil || !strings.Contains(err.Error(), "wlan0") {
				t.Fatalf("a routed network was taken for a success: %v", err)
			}
			if got := calls(t, log); len(got) != 4 || got[3] != "connection delete safehome-console" {
				t.Fatalf("the routed network is left: %q", got)
			}
		})
	}
}

// The dispatcher script runs once the network is up, after nmcli returned: Up waits
// for it.
func TestUpWaitsForTheRoutingToStop(t *testing.T) {
	nmcli, log := fakeNMCLI(t)
	c := good(t, nmcli)
	c.Forwarding = forwarding(t, "1")
	h, _ := New(c)
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.WriteFile(filepath.Join(c.Forwarding, "wlan0", "forwarding"), []byte("0\n"), 0o644)
	}()
	if err := h.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := calls(t, log); len(got) != 3 {
		t.Fatalf("calls %q", got)
	}
}
