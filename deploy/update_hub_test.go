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
// it. A command on /usr/local/bin really runs, by bash as the login shell would, on
// $HUB/usr/local/bin (sudo and mv are stubs). A hub whose program says one of
// $FAKE_CRASHING restarts in a loop (activating, never failed); one of $FAKE_MUTE cannot
// even say its version; $FAKE_RESTART makes restarts fail. $FAKE_INSTALL makes install.sh
// fail before it replaces anything ("before"), or after, its mark naming this run ("after").
const fakeHub = `for cmd; do :; done
printf '%s\n' "$cmd" >> "$LOG"
hub=$(cat "$HUB/usr/local/bin/safehome-hub")
among() { case " $1 " in *" $hub "*) return 0 ;; esac; return 1; }
case "$cmd" in
*"safehome-hub version")
	if among "$FAKE_MUTE"; then exit 126; fi
	echo "safehome-hub $hub" ;;
*install.sh)
	if [ "$FAKE_INSTALL" = after ]; then echo "run $$" > "$HUB/usr/local/bin/.safehome-prev.unconfirmed"; fi
	[ -z "$FAKE_INSTALL" ] ;;
*"systemctl restart"*) [ -z "$FAKE_RESTART" ] ;;
*/usr/local/bin*) bash -c "$(printf '%s' "$cmd" | sed "s#/usr/local/bin#$HUB/usr/local/bin#g")" ;;
"findmnt -no FSTYPE /") echo ext4 ;;
*nameserver*) echo 1 ;;
*NTPSynchronized*) echo yes ;;
"date +%s") date +%s ;;
*list-units*)
	if among "$FAKE_CRASHING"; then
		echo "safehome-hub.service loaded activating auto-restart safeHome hub"
	else
		echo "safehome-hub.service loaded active running safeHome hub"
	fi
	echo "safehome-agent.service loaded active running safeHome agent"
	echo "mosquitto.service loaded active running Mosquitto" ;;
*"is-enabled safehome-hub.service"* | *"is-enabled safehome-agent.service"*) echo enabled ;;
*is-enabled*) echo disabled ;;
esac`

// unconfirmed is the mark install.sh leaves while the programs in place are not yet
// confirmed by phase 2 (its .prev are the programs that held).
const unconfirmed = "usr/local/bin/.safehome-prev.unconfirmed"

// Phase 2 of update-hub.sh puts the previous programs back when the new ones do not
// hold, restarts the services of those enabled, then stops; the alarm is left alone.
// Each way the rollback itself may fail is said, never skipped in silence.
func TestUpdateHubPhaseTwoPutsThePreviousProgramsBackWhenTheNewOnesDoNotHold(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	const (
		kept    = "kept"    // nothing put back
		back    = "back"    // every .prev put back
		partial = "partial" // the hub back, the gateway's .prev stuck
	)
	for _, c := range []struct {
		name, installed, crashing, mute, install, mvFails, restart string
		noPrev, marked                                             bool
		state                                                      string
		want                                                       []string
	}{
		{name: "the new hub crashes at start", installed: "0.3.0", crashing: "0.3.0", marked: true, state: back,
			want: []string{"the new programs do not hold; the previous programs are back and their services hold"}},
		{name: "the hub says another version", installed: "0.2.9", marked: true, state: back,
			want: []string{"the hub says safehome-hub 0.2.9; the previous programs are back and their services hold"}},
		{name: "the new hub cannot say its version", installed: "0.3.0", mute: "0.3.0", marked: true, state: back,
			want: []string{"the hub says (version not readable); the previous programs are back and their services hold"}},
		{name: "the previous programs do not hold either", installed: "0.3.0", crashing: "0.3.0 0.2.1", marked: true, state: back,
			want: []string{"the new programs do not hold; the previous programs are back but the services still do not hold"}},
		{name: "a restart fails", installed: "0.3.0", crashing: "0.3.0", restart: "fails", marked: true, state: back,
			want: []string{"!! safehome-hub back, but its restart failed", "the previous programs are back and their services hold"}},
		{name: "a previous program cannot be put back", installed: "0.3.0", crashing: "0.3.0", mvFails: "safehome-gsm.prev", marked: true, state: partial,
			want: []string{"the new programs do not hold, and the previous programs could not all be put back (back: safehome-hub);"}},
		{name: "no previous program was kept", installed: "0.3.0", crashing: "0.3.0", noPrev: true, state: kept,
			want: []string{"the new programs do not hold, and no previous program was kept: nothing to put back"}},
		{name: "install.sh fails after replacing the programs", installed: "0.3.0", install: "after", state: back,
			want: []string{"install.sh failed after replacing the programs; the previous programs are back and their services hold"}},
		{name: "install.sh fails after replacing the programs, a mark left before", installed: "0.3.0", install: "after", marked: true, state: back,
			want: []string{"install.sh failed after replacing the programs; the previous programs are back and their services hold"}},
		{name: "install.sh fails before replacing the programs", installed: "0.3.0", install: "before", state: kept,
			want: []string{"install.sh failed before replacing the programs"}},
		// A phase 2 cut short left its mark: this run replaced nothing, nothing goes back.
		{name: "install.sh fails before replacing the programs, a mark left before", installed: "0.3.0", install: "before", marked: true, state: kept,
			want: []string{"install.sh failed before replacing the programs", "still those of an earlier run, not confirmed"}},
		{name: "the new programs hold", installed: "0.3.0", marked: true, state: kept,
			want: []string{"phase 2 done"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			hub, log, stubs, bundle := filepath.Join(dir, "hub"), filepath.Join(dir, "log"), filepath.Join(dir, "stubs"), filepath.Join(dir, "bundle")
			writeScript(t, filepath.Join(stubs, "ssh"), fakeHub)
			writeScript(t, filepath.Join(stubs, "scp"), "exit 0")
			writeScript(t, filepath.Join(stubs, "sleep"), "exit 0")
			writeScript(t, filepath.Join(stubs, "sudo"), `[ "$1" != -n ] || shift; exec "$@"`)
			writeScript(t, filepath.Join(stubs, "mv"), `if [ -n "$FAKE_MV_FAILS" ] && [ "$2" = "$FAKE_MV_FAILS" ]; then exit 1; fi; exec /bin/mv "$@"`)
			writeScript(t, filepath.Join(bundle, "deploy", "install.sh"), "exit 0")
			bin := filepath.Join(hub, "usr", "local", "bin")
			files := map[string]string{"safehome-hub": c.installed, "safehome-gsm": "new gsm", "safehome-agent": "agent"}
			if !c.noPrev {
				files["safehome-hub.prev"], files["safehome-gsm.prev"] = "0.2.1", "old gsm"
			}
			files["safehome-video.prev.new"] = "a copy cut short" // never a way back
			if c.marked {
				files[strings.TrimPrefix(unconfirmed, "usr/local/bin/")] = "run cut short"
			}
			for name, content := range files {
				writeFile(t, filepath.Join(bin, name), content)
			}

			cmd := exec.Command("bash", "update-hub.sh", "-H", "admin@rig", "-k", filepath.Join(dir, "key"),
				"-w", filepath.Join(dir, "work"), "-v", "0.3.0", "2", bundle)
			cmd.Env = append(os.Environ(), "LOG="+log, "HUB="+hub, "FAKE_CRASHING="+c.crashing, "FAKE_MUTE="+c.mute,
				"FAKE_INSTALL="+c.install, "FAKE_RESTART="+c.restart, "FAKE_MV_FAILS="+c.mvFails,
				"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
			b, err := cmd.CombinedOutput()
			out := string(b)
			holds := c.want[0] == "phase 2 done"
			if (err == nil) != holds {
				t.Fatalf("err = %v, want an error: %v, in:\n%s", err, !holds, out)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("%q not in:\n%s", w, out)
				}
			}
			cmds := strings.Split(string(must(os.ReadFile(log))), "\n")
			put := slices.IndexFunc(cmds, func(l string) bool { return strings.Contains(l, "safehome-*.prev") })
			reload := slices.Index(cmds, "sudo -n systemctl daemon-reload")
			restart := slices.Index(cmds, "sudo -n systemctl restart safehome-hub.service")
			for _, l := range cmds {
				if strings.Contains(l, "engine.json") || strings.Contains(l, "disarm") ||
					strings.Contains(l, "restart safehome-gsm") {
					t.Errorf("ran %q: the alarm and a disabled service are left alone", l)
				}
			}
			got := map[string]string{}
			for _, name := range []string{"safehome-hub", "safehome-hub.prev", "safehome-gsm", "safehome-gsm.prev"} {
				b, _ := os.ReadFile(filepath.Join(bin, name))
				got[name] = string(b)
			}
			want := map[string]string{"safehome-hub": "0.2.1", "safehome-hub.prev": "", "safehome-gsm": "old gsm", "safehome-gsm.prev": ""}
			switch c.state {
			case kept:
				want = map[string]string{"safehome-hub": c.installed, "safehome-hub.prev": files["safehome-hub.prev"],
					"safehome-gsm": "new gsm", "safehome-gsm.prev": files["safehome-gsm.prev"]}
				if put >= 0 && !c.noPrev {
					t.Errorf("put the previous programs back: %q", cmds[put])
				}
			case partial:
				want["safehome-gsm"], want["safehome-gsm.prev"] = "new gsm", "old gsm"
				if reload >= 0 {
					t.Errorf("went on after a .prev could not be put back: %q", cmds[reload])
				}
			case back:
				if put < 0 || reload < put || restart < reload {
					t.Errorf("put back at %d, daemon-reload at %d, hub restarted at %d: want them in this order", put, reload, restart)
				}
			}
			if _, err := os.Stat(filepath.Join(bin, "safehome-video")); err == nil {
				t.Error("a .prev.new, a copy cut short, was put back")
			}
			if !maps.Equal(got, want) {
				t.Errorf("programs = %v, want %v", got, want)
			}
			// Gone once nothing is pending: the new programs held, or every .prev is back.
			_, err = os.Stat(filepath.Join(hub, unconfirmed))
			if pending := err == nil; pending != (c.marked && (c.state == partial || (c.state == kept && !holds))) {
				t.Errorf("mark %s left: %v", unconfirmed, pending)
			}
		})
	}
}
