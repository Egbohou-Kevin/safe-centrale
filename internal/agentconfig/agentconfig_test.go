package agentconfig

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"safehome/internal/agent"
	"safehome/internal/agentapp"
)

// example copies the example configuration next to a secret, in a temporary folder.
func example(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "agent.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cloud.secret"), []byte("s3cret-of-the-site\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheExampleIsValidAndRepeatsTheDefaults(t *testing.T) {
	r, err := Load(example(t))
	if err != nil {
		t.Fatal(err)
	}
	c := r.Cloud
	if len(c.URLs) != 2 || !strings.HasPrefix(c.URLs[0], "mqtts://") || !strings.HasPrefix(c.URLs[1], "wss://") {
		t.Fatalf("%v", c.URLs)
	}
	// The photos go only to the cloud's hosts: the relay knows them.
	want := agentapp.DefaultConfig("tg-lom-0001")
	want.Relay.CloudURLs = c.URLs
	if !reflect.DeepEqual(r.App, want) {
		t.Fatalf("the example differs from the defaults:\n%+v\n%+v", r.App, want)
	}
	if c.Password != "s3cret-of-the-site" || c.Username != "tg-lom-0001" || c.CAFile != "" {
		t.Fatalf("%+v", c)
	}
	if r.BrokerURL != "mqtt://127.0.0.1:1883" || r.StateDir != "/var/lib/safehome" || r.LogLevel != slog.LevelInfo {
		t.Fatalf("%+v", r)
	}
}

func TestThePasswordNeverShows(t *testing.T) {
	r, err := Load(example(t))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("cloud", "cloud", r.Cloud)
	for _, s := range []string{fmt.Sprint(r.Cloud), fmt.Sprintf("%+v", r.Cloud), fmt.Sprintf("%#v", r.Cloud), fmt.Sprintf("%v", r), logs.String()} {
		if strings.Contains(s, "s3cret") {
			t.Errorf("the password shows in %q", s)
		}
	}
}

// write puts a configuration and a secret in a temporary folder.
func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cloud.secret"), []byte("pw"), 0o600)
	os.WriteFile(filepath.Join(dir, "empty.secret"), []byte(" \n"), 0o600)
	os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("-"), 0o600)
	path := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func cfg(cloud, extra string) string {
	return `{"site": "tg-lom-0001", "state_dir": "/tmp/s", "cloud": {` + cloud + `}` + extra + `}`
}

const goodCloud = `"urls": ["mqtts://cloud.example:8883"], "password_file": "cloud.secret"`

func TestDefaultsAndOverrides(t *testing.T) {
	r, err := Load(write(t, cfg(goodCloud+`, "ca_file": "ca.pem"`,
		`, "status_every": "30s", "confirm_wait": "5s", "queue": {"max_items": 10, "max_bytes": 2048, "max_age": "1h"}, "down": {"burst": 5, "per_minute": 10}, "log_level": "debug"`)))
	if err != nil {
		t.Fatal(err)
	}
	a := r.App
	if a.StatusEvery != 30*time.Second || a.Relay.ConfirmWait != 5*time.Second || a.Relay.QueueItems != 10 ||
		a.Relay.QueueBytes != 2048 || a.Relay.QueueAge != time.Hour || a.Relay.DownBurst != 5 || a.Relay.DownPerMinute != 10 {
		t.Fatalf("%+v", a)
	}
	if r.ClientID != "safehome-agent-tg-lom-0001" || r.Cloud.ClientID != "safehome-agent-tg-lom-0001" || r.Cloud.Username != "tg-lom-0001" {
		t.Fatalf("%+v", r)
	}
	if !filepath.IsAbs(r.Cloud.CAFile) || filepath.Base(r.Cloud.CAFile) != "ca.pem" || r.LogLevel != slog.LevelDebug {
		t.Fatalf("%+v", r.Cloud)
	}
}

func TestMobileDataSavesTheClientsAllowance(t *testing.T) {
	for uplink, want := range map[string]time.Duration{"": 0, `, "uplink": "fixed"`: 0, `, "uplink": "mobile"`: agent.EconomyEvery} {
		r, err := Load(write(t, cfg(goodCloud, uplink)))
		if err != nil {
			t.Fatal(err)
		}
		if r.App.Relay.Economy != want {
			t.Errorf("%q: economy %v, want %v", uplink, r.App.Relay.Economy, want)
		}
	}
}

func TestMistakesAreRefused(t *testing.T) {
	for want, body := range map[string]string{
		"cloud.urls is required":    `{"site": "tg-lom-0001", "state_dir": "/tmp/s"}`,
		"not encrypted":             cfg(`"urls": ["mqtt://cloud.example:1883"], "password_file": "cloud.secret"`, ""),
		"not encrypted, use":        cfg(`"urls": ["mqtts://cloud.example:8883", "ws://cloud.example:80"], "password_file": "cloud.secret"`, ""),
		"no credentials":            cfg(`"urls": ["mqtts://site:pw@cloud.example:8883"], "password_file": "cloud.secret"`, ""),
		"invalid URL":               cfg(`"urls": ["mqtts://"], "password_file": "cloud.secret"`, ""),
		"password_file is required": cfg(`"urls": ["mqtts://cloud.example:8883"]`, ""),
		"password_file is empty":    cfg(`"urls": ["mqtts://cloud.example:8883"], "password_file": "empty.secret"`, ""),
		"cloud.password_file:":      cfg(`"urls": ["mqtts://cloud.example:8883"], "password_file": "missing.secret"`, ""),
		"cloud.ca_file":             cfg(goodCloud+`, "ca_file": "missing.pem"`, ""),
		"state_dir is required":     `{"site": "tg-lom-0001", "cloud": {` + goodCloud + `}}`,
		`unknown field "pasword"`:   cfg(goodCloud, `, "pasword": "x"`),
		"status_every":              cfg(goodCloud, `, "status_every": "often"`),
		"queue.max_age":             cfg(goodCloud, `, "queue": {"max_age": "forever"}`),
		"log_level":                 cfg(goodCloud, `, "log_level": "loud"`),
		"invalid site":              `{"site": "X", "state_dir": "/tmp/s", "cloud": {` + goodCloud + `}}`,
		"trailing data":             cfg(goodCloud, "") + `{}`,
		`uplink "4g"`:               cfg(goodCloud, `, "uplink": "4g"`),
	} {
		_, err := Load(write(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error with %q", body, err, want)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Error("a missing file was accepted")
	}
}

// A password written in a cloud URL by mistake never reaches an error: errors are
// logged.
func TestACloudURLErrorNeverRepeatsAPassword(t *testing.T) {
	for _, u := range []string{
		"mqtt://site:pw123@cloud.example:1883",
		"mqtts://site:pw123@",
		"mqtts://site:pw123@cloud.example:8883",
		"mqtts://site:pw123@cloud.example:port",
	} {
		_, err := Load(write(t, cfg(`"urls": ["`+u+`"], "password_file": "cloud.secret"`, "")))
		if err == nil || strings.Contains(err.Error(), "pw123") || !strings.Contains(err.Error(), "cloud.urls[0]") {
			t.Errorf("%s: %v", u, err)
		}
	}
}
