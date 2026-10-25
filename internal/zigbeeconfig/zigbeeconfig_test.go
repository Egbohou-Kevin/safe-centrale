package zigbeeconfig

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"safehome/internal/zigbee"
	"safehome/internal/zigbeeapp"
)

func TestTheExampleFileAndProfilesAreValidAndRepeatTheDefaults(t *testing.T) {
	r, err := Load(filepath.Join("..", "..", "config", "zigbee.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	def := zigbeeapp.DefaultConfig("tg-lom-0001", zigbee.DefaultConfig(nil))
	z := r.App.Zigbee
	if z.ConfirmWait != def.Zigbee.ConfirmWait || z.Attempts != def.Zigbee.Attempts || z.OfflineAfter != def.Zigbee.OfflineAfter ||
		z.PairMax != def.Zigbee.PairMax || z.Step != def.Zigbee.Step || r.App.StatusEvery != def.StatusEvery || r.App.Base != def.Base {
		t.Fatalf("the example differs from the defaults: %+v", r.App)
	}
	if len(z.Profiles) != 5 || r.StateDir == "" || r.ClientID != "safehome-zigbee-tg-lom-0001" {
		t.Fatalf("%+v", r)
	}
	names := map[string]bool{}
	for _, p := range z.Profiles {
		names[p.Name] = true
		// The motion sensor never says "no movement": Zigbee2MQTT does, by itself.
		if rest := p.Capabilities["motion"].Rest; p.Name == "aqara.motion.rtcgq11lm" && rest != "clear" {
			t.Errorf("the motion sensor rests at %q", rest)
		}
	}
	for _, want := range []string{"aqara.contact.mccgq11lm", "aqara.motion.rtcgq11lm", "heiman.smoke.hs1sa", "heiman.siren.hs2wd", "develco.keypad.kepzb110"} {
		if !names[want] {
			t.Errorf("profile %s missing", want)
		}
	}
}

const profile = `{"name": "p", "models": ["M"], "capabilities": {"contact": {"from": "contact", "map": {"true": "closed", "false": "open"}}}}`

// dir writes a configuration and its profiles in a temporary folder.
func dir(t *testing.T, config string, profiles ...string) string {
	t.Helper()
	d := t.TempDir()
	os.Mkdir(filepath.Join(d, "profiles"), 0o755)
	for i, p := range profiles {
		os.WriteFile(filepath.Join(d, "profiles", string(rune('a'+i))+".json"), []byte(p), 0o600)
	}
	path := filepath.Join(d, "zigbee.json")
	os.WriteFile(path, []byte(config), 0o600)
	return path
}

func TestDefaultsAndOverrides(t *testing.T) {
	r, err := Load(dir(t, `{"site": "tg-lom-0001", "state_dir": "/s", "profiles_dir": "profiles"}`, profile))
	if err != nil {
		t.Fatal(err)
	}
	if r.BrokerURL != "mqtt://127.0.0.1:1883" || r.ClientID != "safehome-zigbee-tg-lom-0001" || r.LogLevel != slog.LevelInfo || r.App.Base != "zigbee2mqtt" {
		t.Fatalf("%+v", r)
	}
	r, err = Load(dir(t, `{"site": "tg-lom-0001", "state_dir": "/s", "profiles_dir": "profiles", "broker_url": "mqtt://10.0.0.2:1883",
		"client_id": "zb", "log_level": "debug", "base_topic": "z2m", "confirm_wait": "5s", "attempts": 2, "offline_after": "6h",
		"pair_max": "5m", "step": 10, "status_every": "30s"}`, profile))
	if err != nil {
		t.Fatal(err)
	}
	z := r.App.Zigbee
	if r.ClientID != "zb" || r.LogLevel != slog.LevelDebug || r.App.Base != "z2m" || z.ConfirmWait != 5*time.Second || z.Attempts != 2 ||
		z.OfflineAfter != 6*time.Hour || z.PairMax != 5*time.Minute || z.Step != 10 || r.App.StatusEvery != 30*time.Second {
		t.Fatalf("%+v", r)
	}
	// An absolute profiles_dir is taken as it is.
	abs := filepath.Join(filepath.Dir(dir(t, "{}", profile)), "profiles")
	if _, err := Load(dir(t, `{"site": "tg-lom-0001", "state_dir": "/s", "profiles_dir": "`+abs+`"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestMistakesInTheConfiguration(t *testing.T) {
	for name, body := range map[string]string{
		"not json":       `{`,
		"trailing":       `{"site": "tg-lom-0001", "state_dir": "/s", "profiles_dir": "profiles"} {}`,
		"unknown key":    `{"site": "tg-lom-0001", "state_dir": "/s", "profiles_dir": "profiles", "atempts": 2}`,
		"no state dir":   `{"site": "tg-lom-0001", "profiles_dir": "profiles"}`,
		"no profiles":    `{"site": "tg-lom-0001", "state_dir": "/s"}`,
		"bad site":       `{"site": "Lome", "state_dir": "/s", "profiles_dir": "profiles"}`,
		"bad duration":   `{"site": "tg-lom-0001", "state_dir": "/s", "profiles_dir": "profiles", "confirm_wait": "soon"}`,
		"bad status":     `{"site": "tg-lom-0001", "state_dir": "/s", "profiles_dir": "profiles", "status_every": "x"}`,
		"zero attempts":  `{"site": "tg-lom-0001", "state_dir": "/s", "profiles_dir": "profiles", "attempts": 0}`,
		"bad base":       `{"site": "tg-lom-0001", "state_dir": "/s", "profiles_dir": "profiles", "base_topic": "z/#"}`,
		"bad log level":  `{"site": "tg-lom-0001", "state_dir": "/s", "profiles_dir": "profiles", "log_level": "loud"}`,
		"empty profiles": `{"site": "tg-lom-0001", "state_dir": "/s", "profiles_dir": "nowhere"}`,
	} {
		if _, err := Load(dir(t, body, profile)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file")
	}
	_, err := Load(dir(t, `{"site": "tg-lom-0001", "state_dir": "/s", "profiles_dir": "profiles"}`, profile, `{"name": "q"`))
	if err == nil || !strings.Contains(err.Error(), "b.json") {
		t.Errorf("the faulty profile is named: %v", err)
	}
}

func TestAProfileIsReadWithItsCommands(t *testing.T) {
	p, err := ParseProfile([]byte(`{"name": "heiman.siren.hs2wd", "models": ["HS2WD-E"], "note": "for people",
		"capabilities": {"siren": {"from": "alarm", "map": {"ON": "on", "OFF": "off"}}, "link": {"from": "linkquality", "max": 255, "step": 20}},
		"commands": {"siren.on": {"payload": {"warning": {"duration": "{duration_s}"}}, "confirm": {"capability": "siren", "value": "on"}}},
		"offline_after": "2h"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.OfflineAfter != 2*time.Hour || p.Capabilities["link"].Max != 255 || p.Capabilities["link"].Step != 20 ||
		p.Commands["siren.on"].Confirm.Value != "on" || p.Commands["siren.on"].Payload["warning"] == nil {
		t.Fatalf("%+v", p)
	}
}

func TestProfilesAreCheckedAgainstTheContract(t *testing.T) {
	for name, body := range map[string]string{
		"not json":            `{`,
		"unknown key":         `{"name": "p", "models": ["M"], "capabilities": {"contact": {"from": "contact", "mapp": {}}}}`,
		"value not allowed":   `{"name": "p", "models": ["M"], "capabilities": {"contact": {"from": "contact", "map": {"true": "shut"}}}}`,
		"unknown capability":  `{"name": "p", "models": ["M"], "capabilities": {"teleport": {"from": "x", "map": {"a": "b"}}}}`,
		"not a number":        `{"name": "p", "models": ["M"], "capabilities": {"contact": {"from": "contact"}}}`,
		"adapter's own":       `{"name": "p", "models": ["M"], "capabilities": {"availability": {"from": "x", "map": {"a": "online"}}}}`,
		"hub's own":           `{"name": "p", "models": ["M"], "capabilities": {"mains": {"from": "x", "map": {"a": "lost"}}}}`,
		"keypad as a state":   `{"name": "p", "models": ["M"], "capabilities": {"keypad": {"from": "action", "map": {"disarm": "disarm"}}}}`,
		"panic as a state":    `{"name": "p", "models": ["M"], "capabilities": {"panic": {"from": "action", "map": {"x": "pressed"}}}}`,
		"code off a keypad":   `{"name": "p", "models": ["M"], "capabilities": {"panic": {"from": "action", "map": {"x": "pressed"}, "momentary": true, "code_from": "c"}}}`,
		"unknown command":     `{"name": "p", "models": ["M"], "capabilities": {"link": {"from": "linkquality"}}, "commands": {"door.open": {"payload": {"a": 1}}}}`,
		"empty command":       `{"name": "p", "models": ["M"], "capabilities": {"link": {"from": "linkquality"}}, "commands": {"siren.on": {}}}`,
		"bad offline":         `{"name": "p", "models": ["M"], "capabilities": {"link": {"from": "linkquality"}}, "offline_after": "later"}`,
		"no model":            `{"name": "p", "capabilities": {"link": {"from": "linkquality"}}}`,
		"confirm a raw value": `{"name": "p", "models": ["M"], "capabilities": {"siren": {"from": "alarm", "map": {"ON": "on", "OFF": "off"}}}, "commands": {"siren.on": {"payload": {"a": 1}, "confirm": {"capability": "siren", "value": "ON"}}}}`,
		"rest never read":     `{"name": "p", "models": ["M"], "capabilities": {"motion": {"from": "occupancy", "map": {"true": "detected"}, "rest": "clear"}}}`,
		"confirm unknown cap": `{"name": "p", "models": ["M"], "capabilities": {"link": {"from": "linkquality"}}, "commands": {"siren.off": {"payload": {"a": 1}, "confirm": {"capability": "siren", "value": "off"}}}}`,
	} {
		if _, err := ParseProfile([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Two profiles for the same model are refused when the configuration is read.
	_, err := Load(dir(t, `{"site": "tg-lom-0001", "state_dir": "/s", "profiles_dir": "profiles"}`, profile,
		`{"name": "q", "models": ["M"], "capabilities": {"link": {"from": "linkquality"}}}`))
	if err == nil {
		t.Error("one model, two profiles")
	}
}
