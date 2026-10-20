package gsmconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"safehome/internal/gateway"
	"safehome/internal/gsm"
)

const minimal = `{"site": "tg-lom-0001", "state_dir": "/tmp/s", "lines": [{"id": "line1", "port": "/dev/ttyUSB2"}]}`

func TestTheExampleFileIsValidAndRepeatsTheDefaults(t *testing.T) {
	r, err := Load(filepath.Join("..", "..", "config", "gsm.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	g := r.Gateway.GSM
	def := gsm.DefaultConfig(nil, nil)
	if g.RingFor != def.RingFor || g.CheckEvery != def.CheckEvery || g.TestEvery != def.TestEvery ||
		g.MaxAge[gsm.PriorityIntrusion] != def.MaxAge[gsm.PriorityIntrusion] || g.MaxAge[gsm.PriorityOther] != def.MaxAge[gsm.PriorityOther] {
		t.Fatalf("the example differs from the defaults: %+v", g)
	}
	if r.Gateway.StatusEvery != gateway.DefaultConfig("x", def).StatusEvery {
		t.Fatalf("status_every %v differs from the default", r.Gateway.StatusEvery)
	}
	if len(g.Contacts) != 4 || g.TestNumber == "" || r.Ports["line1"].Path != "/dev/ttyUSB2" || !r.Ports["line1"].Configure {
		t.Fatalf("%+v %+v", g, r.Ports)
	}
}

func TestDefaultsAndOverrides(t *testing.T) {
	r, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if r.BrokerURL != "mqtt://127.0.0.1:1883" || r.ClientID != "safehome-gsm-tg-lom-0001" || r.Ports["line1"].Baud != 115200 || r.Gateway.GSM.Country != "228" {
		t.Fatalf("%+v", r)
	}
	r, err = Parse([]byte(`{"site": "tg-lom-0001", "state_dir": "/tmp/s", "log_level": "debug", "country": "229",
		"broker_url": "mqtt://10.0.0.2:1883", "client_id": "gw",
		"lines": [{"id": "a", "port": "/dev/a", "baud": 9600, "configure_port": false}, {"id": "b", "port": "/dev/b", "operator": "moov"}],
		"ring_for": "30s", "status_every": "20s", "max_age": {"life_safety": "5m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Ports["a"].Baud != 9600 || r.Ports["a"].Configure || len(r.Gateway.GSM.Lines) != 2 || r.Gateway.GSM.Lines[1].Operator != "moov" ||
		r.Gateway.GSM.RingFor != 30*time.Second || r.Gateway.GSM.MaxAge[gsm.PriorityLifeSafety] != 5*time.Minute ||
		r.Gateway.GSM.MaxAge[gsm.PriorityOther] != time.Hour || r.Gateway.StatusEvery != 20*time.Second || r.Gateway.GSM.Country != "229" || r.ClientID != "gw" {
		t.Fatalf("%+v", r)
	}
}

func TestMistakes(t *testing.T) {
	for name, body := range map[string]string{
		"not json":          `{`,
		"trailing":          minimal + `{}`,
		"unknown key":       `{"site": "tg-lom-0001", "state_dir": "/s", "lines": [{"id": "l", "port": "/p"}], "ring": "2s"}`,
		"no state dir":      `{"site": "tg-lom-0001", "lines": [{"id": "l", "port": "/p"}]}`,
		"no line":           `{"site": "tg-lom-0001", "state_dir": "/s"}`,
		"no port":           `{"site": "tg-lom-0001", "state_dir": "/s", "lines": [{"id": "l"}]}`,
		"bad site":          `{"site": "Lome", "state_dir": "/s", "lines": [{"id": "l", "port": "/p"}]}`,
		"bad number":        `{"site": "tg-lom-0001", "state_dir": "/s", "lines": [{"id": "l", "port": "/p"}], "contacts": {"u": "abc"}}`,
		"bad duration":      `{"site": "tg-lom-0001", "state_dir": "/s", "lines": [{"id": "l", "port": "/p"}], "ring_for": "long"}`,
		"bad status every":  `{"site": "tg-lom-0001", "state_dir": "/s", "lines": [{"id": "l", "port": "/p"}], "status_every": "often"}`,
		"zero status every": `{"site": "tg-lom-0001", "state_dir": "/s", "lines": [{"id": "l", "port": "/p"}], "status_every": "0s"}`,
		"bad max age":       `{"site": "tg-lom-0001", "state_dir": "/s", "lines": [{"id": "l", "port": "/p"}], "max_age": {"other": "x"}}`,
		"zero max age":      `{"site": "tg-lom-0001", "state_dir": "/s", "lines": [{"id": "l", "port": "/p"}], "max_age": {"other": "0s"}}`,
		"bad log level":     `{"site": "tg-lom-0001", "state_dir": "/s", "lines": [{"id": "l", "port": "/p"}], "log_level": "loud"}`,
		"bad country":       `{"site": "tg-lom-0001", "state_dir": "/s", "lines": [{"id": "l", "port": "/p"}], "country": "+228"}`,
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte("{"), 0o600)
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "bad.json") {
		t.Fatalf("%v", err)
	}
	for _, l := range []string{"info", "warn", "warning", "error", ""} {
		if _, err := parseLevel(l); err != nil {
			t.Errorf("%q", l)
		}
	}
}
