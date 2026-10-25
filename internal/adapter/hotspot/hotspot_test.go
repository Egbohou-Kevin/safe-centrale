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

func good(nmcli string) Config {
	return Config{Interface: "wlan0", SSID: "safehome-tg-lom-0001", Passphrase: "k7p2m9x4q8", Address: "10.42.0.1", NMCLI: nmcli}
}

func TestUpMakesAnAccessPointThatNeverStartsAtBoot(t *testing.T) {
	nmcli, log := fakeNMCLI(t)
	h, err := New(good(nmcli))
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
	h, _ := New(good(nmcli))
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
	h, _ := New(good(nmcli))
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
	} {
		c := good("nmcli")
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
	h, _ := New(good(path))
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

// A start cut short by its deadline: NetworkManager may still bring the network up
// after nmcli is killed, so it is deleted all the same, on a time of its own (the
// caller's is over).
func TestAStartPastItsDeadlineIsCleanedUp(t *testing.T) {
	dir := t.TempDir()
	log, path := filepath.Join(dir, "log"), filepath.Join(dir, "nmcli")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$1 $2\" in \"connection up\") exec sleep 5;; esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h, _ := New(good(path))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := h.Up(ctx); err == nil {
		t.Fatal("a start past its deadline was taken for a success")
	}
	if got := calls(t, log); len(got) != 4 || got[3] != "connection delete safehome-console" {
		t.Fatalf("the network may come up and stay: %q", got)
	}
}
