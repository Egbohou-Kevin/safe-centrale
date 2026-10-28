package deploy

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// update-hub.sh, the update of a hub in read-only mode (docs/systeme.md, "Maintenance").

// Phase 4 of update-hub.sh refuses a hub whose clock would start from the day the system
// went read-only: timesyncd's folder bound on the data partition, timesyncd running.
func TestUpdateHubPhaseFourChecksTimesyncdsClockOnTheDataPartition(t *testing.T) {
	var phase4 []string
	in := false
	for _, l := range rawLines(t, "update-hub.sh") {
		switch strings.TrimSpace(l) {
		case "4)":
			in = true
			continue
		case ";;":
			in = false
		}
		if in {
			phase4 = append(phase4, l)
		}
	}
	body := strings.Join(phase4, "\n")
	for _, want := range []string{"findmnt -no FSTYPE " + timesyncdClock + "')\" = ext4", "systemctl is-active systemd-timesyncd"} {
		if !strings.Contains(body, want) {
			t.Errorf("phase 4 does not check %q", want)
		}
	}
}

// fakeHub stands for ssh to the hub in update-hub.sh: it logs each command and answers
// it. The command that puts the .prev back really runs, on $HUB/usr/local/bin; a hub
// whose program says $FAKE_CRASHING restarts in a loop (activating, never failed).
const fakeHub = `for cmd; do :; done
printf '%s\n' "$cmd" >> "$LOG"
hub=$(cat "$HUB/usr/local/bin/safehome-hub")
case "$cmd" in
*.prev*) sh -c "$(printf '%s' "$cmd" | sed -e 's/sudo -n //' -e "s#/usr/local/bin#$HUB/usr/local/bin#")" ;;
"findmnt -no FSTYPE /") echo ext4 ;;
*nameserver*) echo 1 ;;
*NTPSynchronized*) echo yes ;;
"date +%s") date +%s ;;
*"safehome-hub version") echo "safehome-hub $hub" ;;
*list-units*)
	if [ "$hub" = "$FAKE_CRASHING" ]; then
		echo "safehome-hub.service loaded activating auto-restart safeHome hub"
	else
		echo "safehome-hub.service loaded active running safeHome hub"
	fi
	echo "safehome-agent.service loaded active running safeHome agent"
	echo "mosquitto.service loaded active running Mosquitto" ;;
*"is-enabled safehome-hub.service"* | *"is-enabled safehome-agent.service"*) echo enabled ;;
*is-enabled*) echo disabled ;;
esac`

// Phase 2 of update-hub.sh puts the previous programs back when the new ones do not
// hold, restarts the services of those enabled, then stops; the alarm is left alone.
func TestUpdateHubPhaseTwoPutsThePreviousProgramsBackWhenTheNewOnesDoNotHold(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	for _, c := range []struct {
		name, installed, crashing, want string
		backs                           bool
	}{
		{"the new hub crashes at start", "0.3.0", "0.3.0", "the new programs do not hold; the previous programs are back and their services hold", true},
		{"the hub says another version", "0.2.9", "", "the hub says safehome-hub 0.2.9; the previous programs are back and their services hold", true},
		{"the new programs hold", "0.3.0", "", "phase 2 done", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			hub, log, stubs, bundle := filepath.Join(dir, "hub"), filepath.Join(dir, "log"), filepath.Join(dir, "stubs"), filepath.Join(dir, "bundle")
			writeScript(t, filepath.Join(stubs, "ssh"), fakeHub)
			writeScript(t, filepath.Join(stubs, "scp"), "exit 0")
			writeScript(t, filepath.Join(stubs, "sleep"), "exit 0")
			writeScript(t, filepath.Join(bundle, "deploy", "install.sh"), "exit 0")
			bin := filepath.Join(hub, "usr", "local", "bin")
			for name, content := range map[string]string{
				"safehome-hub": c.installed, "safehome-hub.prev": "0.2.1",
				"safehome-gsm": "new gsm", "safehome-gsm.prev": "old gsm", "safehome-agent": "agent",
			} {
				writeFile(t, filepath.Join(bin, name), content)
			}

			cmd := exec.Command("bash", "update-hub.sh", "-H", "admin@rig", "-k", filepath.Join(dir, "key"),
				"-w", filepath.Join(dir, "work"), "-v", "0.3.0", "2", bundle)
			cmd.Env = append(os.Environ(), "LOG="+log, "HUB="+hub, "FAKE_CRASHING="+c.crashing,
				"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
			b, err := cmd.CombinedOutput()
			out := string(b)
			if (err != nil) != c.backs || !strings.Contains(out, c.want) {
				t.Fatalf("err = %v, want an error: %v, and %q in:\n%s", err, c.backs, c.want, out)
			}
			cmds := strings.Split(string(must(os.ReadFile(log))), "\n")
			put := slices.IndexFunc(cmds, func(l string) bool { return strings.Contains(l, ".prev") })
			reload := slices.Index(cmds, "sudo -n systemctl daemon-reload")
			restart := slices.Index(cmds, "sudo -n systemctl restart safehome-hub.service")
			for _, l := range cmds {
				if strings.Contains(l, "engine.json") || strings.Contains(l, "disarm") ||
					strings.Contains(l, "restart safehome-gsm") {
					t.Errorf("ran %q: the alarm and a disabled service are left alone", l)
				}
			}
			kept := map[string]string{}
			for _, name := range []string{"safehome-hub", "safehome-hub.prev", "safehome-gsm", "safehome-gsm.prev"} {
				b, _ := os.ReadFile(filepath.Join(bin, name))
				kept[name] = string(b)
			}
			if !c.backs {
				if put >= 0 || kept["safehome-hub.prev"] != "0.2.1" {
					t.Errorf("put the previous programs back though the new ones hold: %q, %v", cmds[max(put, 0)], kept)
				}
				return
			}
			if put < 0 || reload < put || restart < reload {
				t.Errorf("put back at %d, daemon-reload at %d, hub restarted at %d: want them in this order", put, reload, restart)
			}
			want := map[string]string{"safehome-hub": "0.2.1", "safehome-hub.prev": "", "safehome-gsm": "old gsm", "safehome-gsm.prev": ""}
			if !maps.Equal(kept, want) {
				t.Errorf("programs = %v, want %v", kept, want)
			}
		})
	}
}
