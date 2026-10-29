package deploy

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Phase 1 announces the update before anything stops (the announcement written whole
// in the hub's state folder, then the hub's journal read until it says it, 30 s at
// most), and never stops for it: a hub that does not say it (one from before the
// announcement) is said, and the update goes on. Phase 4 withdraws it once the hub
// holds; a phase 4 that stops leaves it to expire.
func TestUpdateHubAnnouncesTheUpdateBeforeAnythingStops(t *testing.T) {
	const readOnly = "console=serial0,115200 root=PARTUUID=1-02 overlayroot=tmpfs:recurse=0 rootwait\n"
	for _, c := range []struct {
		name string
		env  []string
		want string
	}{
		{"the hub says it", nil, "   said by the hub"},
		{"the hub does not say it", []string{"FAKE_UNANNOUNCED=1"},
			"!! the hub has not said the update after 30 s (a hub from before 29 October 2026 knows no announcement)"},
	} {
		t.Run("phase 1, "+c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "hub", "boot", "firmware", "cmdline.txt"), readOnly)
			cmd, log := updateHub(t, dir, map[string]string{"safehome-hub": "0.3.0"}, "1")
			cmd.Env = append(cmd.Env, c.env...)
			b, err := cmd.CombinedOutput()
			out := string(b)
			if err != nil || !strings.Contains(out, "phase 1 done") || !strings.Contains(out, c.want) {
				t.Fatalf("err = %v, want phase 1 done and %q, in:\n%s", err, c.want, out)
			}
			ends := strings.TrimSpace(string(must(os.ReadFile(filepath.Join(dir, "hub", "var", "lib", "safehome", "updating-until")))))
			at, err := time.Parse(time.RFC3339, ends)
			if err != nil {
				t.Fatalf("the announcement holds %q: %v", ends, err)
			}
			if d := time.Until(at); d < 19*time.Minute || d > 20*time.Minute {
				t.Errorf("announced until %s, %v from now, want 20 minutes", ends, d)
			}
			cmds := strings.Split(string(must(os.ReadFile(log))), "\n")
			first := func(sub string) int {
				return slices.IndexFunc(cmds, func(l string) bool { return strings.Contains(l, sub) })
			}
			written, asked, reboot := first("tee /var/lib/safehome/updating-until.new"), first("sed -i 's/overlayroot"), first("sudo -n reboot")
			if written < 0 || written > asked || written > reboot {
				t.Errorf("announcement written at command %d, cmdline.txt changed at %d, reboot at %d: it goes first", written, asked, reboot)
			}
			read := 0
			for _, l := range cmds {
				if strings.Contains(l, `grep -qF 'msg="update announced"'`) {
					read++
				}
			}
			if want := map[bool]int{true: 10, false: 1}[c.env != nil]; read != want {
				t.Errorf("the hub's journal read %d times, want %d", read, want)
			}
		})
	}
	for _, c := range []struct {
		name, want string
		env        []string
		withdrawn  bool
	}{
		{"the hub holds", "== update announcement withdrawn", nil, true},
		{"the agent has not reached the cloud", "the agent has not reached the cloud", []string{"FAKE_NO_CLOUD=1"}, false},
	} {
		t.Run("phase 4, "+c.name, func(t *testing.T) {
			dir := t.TempDir()
			announced := filepath.Join(dir, "hub", "var", "lib", "safehome", "updating-until")
			writeFile(t, announced, "2026-10-29T20:53:00Z\n")
			writeFile(t, filepath.Join(dir, "work", "mode"), "armed_home\n")
			cmd, _ := updateHub(t, dir, map[string]string{"safehome-hub": "0.3.0"}, "4")
			cmd.Env = append(append(cmd.Env, "FAKE_ROOT_FS=overlay"), c.env...)
			b, err := cmd.CombinedOutput()
			out := string(b)
			if (err == nil) != c.withdrawn || !strings.Contains(out, c.want) {
				t.Fatalf("err = %v, want %q, in:\n%s", err, c.want, out)
			}
			if _, err := os.Stat(announced); (err != nil) != c.withdrawn {
				t.Errorf("announcement withdrawn: %v, want %v", err != nil, c.withdrawn)
			}
		})
	}
}
