package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cli(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "power.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Without the module (this computer), check says the configuration is good and that
// the module cannot be reached, without failing: the installation comes before it.
func TestCheckSaysTheConfigurationAndTheModule(t *testing.T) {
	cfg := writeConfig(t, `{"site":"tg-lom-0001","state_dir":"`+t.TempDir()+`","device_id":"dev_hubpower","gpio_chip":"no-such-chip"}`)
	code, out, errs := cli("check", "-config", cfg)
	if code != 0 || !strings.Contains(out, "configuration OK: site tg-lom-0001, device dev_hubpower") ||
		!strings.Contains(errs, "cannot be reached") {
		t.Fatalf("code %d\n%s\n%s", code, out, errs)
	}
}

func TestABadConfigurationIsRefused(t *testing.T) {
	cfg := writeConfig(t, `{"site":"tg-lom-0001","state_dir":"/s"}`)
	if code, _, errs := cli("check", "-config", cfg); code != 1 || !strings.Contains(errs, "configuration error") {
		t.Fatalf("code %d: %s", code, errs)
	}
}

// The service does not start without its module: systemd starts it again later.
func TestTheServiceNeedsItsModule(t *testing.T) {
	cfg := writeConfig(t, `{"site":"tg-lom-0001","state_dir":"`+t.TempDir()+`","device_id":"dev_hubpower","gpio_chip":"no-such-chip"}`)
	if code, _, errs := cli("run", "-config", cfg); code != 1 || !strings.Contains(errs, "cannot reach the battery module") {
		t.Fatalf("code %d: %s", code, errs)
	}
}
