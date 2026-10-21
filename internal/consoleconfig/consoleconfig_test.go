package consoleconfig

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write puts a configuration and its passphrase file in a folder.
func write(t *testing.T, body, passphrase string) string {
	t.Helper()
	dir := t.TempDir()
	if passphrase != "" {
		if err := os.WriteFile(filepath.Join(dir, "console.secret"), []byte(passphrase), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "console.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func cfg(extra string) string {
	return fmt.Sprintf(`{"site": "tg-lom-0001", "state_dir": "/tmp/s", "hub_config": "/etc/safehome/hub.json",
		"wifi": {"passphrase_file": "console.secret"}%s}`, extra)
}

func TestTheExampleIsValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "console.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(filepath.Join("..", "..", "config", "console.secret.example"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Load(write(t, string(data), string(secret)))
	if err != nil {
		t.Fatal(err)
	}
	if r.Wifi.Interface != "wlan0" || r.Wifi.Address != "10.42.0.1" || r.Port != 80 || r.App.PairFor != 2*time.Minute {
		t.Fatalf("%+v", r)
	}
	if r.Listen() != "10.42.0.1:80" || r.Host() != "10.42.0.1" {
		t.Fatalf("listen %s, host %s", r.Listen(), r.Host())
	}
}

func TestDefaults(t *testing.T) {
	r, err := Load(write(t, cfg(`, "port": 8080`), "  k7p2m9x4q8wd\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Wifi.SSID != "safehome-tg-lom-0001" || r.Wifi.Passphrase != "k7p2m9x4q8wd" || r.ClientID != "safehome-console-tg-lom-0001" ||
		r.BrokerURL != "mqtt://127.0.0.1:1883" || r.Host() != "10.42.0.1:8080" || r.LogLevel != slog.LevelInfo {
		t.Fatalf("%+v", r)
	}
	var b strings.Builder
	slog.New(slog.NewTextHandler(&b, nil)).Info("x", "wifi", r.Wifi)
	if strings.Contains(b.String(), "k7p2m9x4q8wd") {
		t.Fatal("the passphrase shows in the logs")
	}
}

func TestMistakesAreRefused(t *testing.T) {
	for want, c := range map[string]struct{ body, pass string }{
		"state_dir is required":         {`{"site": "tg-lom-0001", "hub_config": "x"}`, "k7p2m9x4q8wd"},
		"hub_config is required":        {`{"site": "tg-lom-0001", "state_dir": "/tmp/s"}`, "k7p2m9x4q8wd"},
		"wifi is required":              {`{"site": "tg-lom-0001", "state_dir": "/tmp/s", "hub_config": "x"}`, "k7p2m9x4q8wd"},
		"invalid site":                  {strings.Replace(cfg(""), "tg-lom-0001", "X", 1), "k7p2m9x4q8wd"},
		"8 to 63 printable":             {cfg(""), "short"},
		"printable ASCII":               {cfg(""), "\u00e9t\u00e9-\u00e0-lom\u00e9"},
		"wifi.passphrase_file:":         {cfg(""), ""},
		"wifi.address":                  {strings.Replace(cfg(""), `"passphrase_file"`, `"address": "fe80::1", "passphrase_file"`, 1), "k7p2m9x4q8wd"},
		"pair_for":                      {cfg(`, "pair_for": "1h"`), "k7p2m9x4q8wd"},
		"port 70000":                    {cfg(`, "port": 70000`), "k7p2m9x4q8wd"},
		"log_level":                     {cfg(`, "log_level": "loud"`), "k7p2m9x4q8wd"},
		`unknown field "pasword"`:       {cfg(`, "pasword": "x"`), "k7p2m9x4q8wd"},
		"trailing data":                 {cfg("") + "{}", "k7p2m9x4q8wd"},
		"wifi.passphrase_file is requi": {strings.Replace(cfg(""), `"passphrase_file": "console.secret"`, `"ssid": "x"`, 1), "k7p2m9x4q8wd"},
	} {
		_, err := Load(write(t, c.body, c.pass))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error with %q", c.body, err, want)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Error("a missing file was accepted")
	}
}
