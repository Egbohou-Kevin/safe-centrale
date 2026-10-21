package powerconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "power.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTheExample(t *testing.T) {
	r, err := Load(filepath.Join("..", "..", "config", "power.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := r.Board
	if r.App.DeviceID != "dev_hubpower" || b.GPIOChip != "pinctrl-bcm2711" || b.MainsLine != 6 || b.CutLine != 26 ||
		b.I2CBus != "/dev/i2c-1" || b.CutPulse != 3*time.Second || r.App.Power.CutMilliVolts != 3000 || r.StateDir != "/var/lib/safehome" {
		t.Fatalf("%+v", r)
	}
}

// The example's device is the hub's: the supervision reads the readings of
// hub_device_id only.
func TestTheExampleTellsAsTheHubsDevice(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "hub.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"hub_device_id": "dev_hubpower"`) {
		t.Fatal("config/hub.example.json and config/power.example.json disagree on the hub's device")
	}
}

func TestDefaultsAndRefusals(t *testing.T) {
	base := `{"site":"tg-lom-0001","state_dir":"/s","device_id":"dev_hubpower"%s}`
	r, err := Load(write(t, strings.Replace(base, "%s", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if r.Board.MainsLine != 6 || r.Board.CutLine != 26 || r.ClientID != "safehome-power-tg-lom-0001" || r.App.PollEvery != time.Second {
		t.Fatalf("%+v", r)
	}
	if r, err := Load(write(t, strings.Replace(base, "%s", `,"cut_line":13,"mains_line":0`, 1))); err != nil || r.Board.CutLine != 13 || r.Board.MainsLine != 0 {
		t.Fatalf("an older board: %+v %v", r.Board, err)
	}
	for name, extra := range map[string]string{
		"same lines":   `,"cut_line":6`,
		"no such line": `,"mains_line":40`,
		"low voltage":  `,"cut_millivolts":2000`,
		"misspelt":     `,"cut_milivolts":3000`,
	} {
		if _, err := Load(write(t, strings.Replace(base, "%s", extra, 1))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Load(write(t, `{"site":"tg-lom-0001","state_dir":"/s"}`)); err == nil || !strings.Contains(err.Error(), "hub_device_id") {
		t.Errorf("no device: %v", err)
	}
}
