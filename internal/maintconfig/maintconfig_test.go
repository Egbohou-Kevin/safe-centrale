package maintconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "maint.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTheExample(t *testing.T) {
	r, err := Load(filepath.Join("..", "..", "config", "maint.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !r.App.Maint.Enabled || r.App.Maint.MaxLength != time.Hour || r.Tunnel.Host != "bastion.safehome.example" ||
		r.Tunnel.RemotePort != 22001 || r.Tunnel.LocalPort != 22 || r.Tunnel.SSH != "/usr/bin/ssh" || r.StateDir != "/var/lib/safehome" {
		t.Fatalf("%+v", r)
	}
}

func TestDefaultsAndRefusals(t *testing.T) {
	base := `{"site":"tg-lom-0001","state_dir":"/s","key_file":"/k","known_hosts":"/kh","bastion":{"host":"b.example","user":"tunnel","remote_port":22001}%s}`
	r, err := Load(write(t, strings.Replace(base, "%s", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if r.Tunnel.Port != 22 || r.BrokerURL != "mqtt://127.0.0.1:1883" || r.ClientID != "safehome-maint-tg-lom-0001" {
		t.Fatalf("%+v", r)
	}
	if r, err := Load(write(t, `{"site":"tg-lom-0001","state_dir":"/s","enabled":false}`)); err != nil || r.App.Maint.Enabled {
		t.Fatalf("switched off without a bastion: %v", err)
	}
	for want, body := range map[string]string{
		"invalid JSON":  `{"site":"tg-lom-0001","state_dir":"/s","bastoin":{}}`,
		"state_dir":     `{"site":"tg-lom-0001"}`,
		"host and user": `{"site":"tg-lom-0001","state_dir":"/s"}`,
		"remote_port":   strings.Replace(base, "22001", "80", 1),
		"key_file":      strings.Replace(base, `"key_file":"/k",`, "", 1),
		"max_length":    strings.Replace(base, "%s", `,"max_length":"forever"`, 1),
		"MaxLength":     strings.Replace(base, "%s", `,"max_length":"3h"`, 1),
		"log_level":     strings.Replace(base, "%s", `,"log_level":"loud"`, 1),
		"invalid site":  strings.Replace(base, "tg-lom-0001", "X", 1),
	} {
		body = strings.Replace(body, "%s", "", 1)
		if _, err := Load(write(t, body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Fatal("missing file")
	}
}
