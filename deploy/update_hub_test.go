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

// fakeHub stands for ssh to the hub in update-hub.sh: it logs each command and answers
// it. A command on /usr/local/bin really runs, by bash as the login shell would, on
// $HUB/usr/local/bin (sudo and mv are stubs). A hub whose program says one of
// $FAKE_CRASHING restarts in a loop (activating, never failed); one of $FAKE_FLAPPING
// crashes a moment after it says it is ready (active, but since half a second, or since
// $FAKE_FLAP_AGE seconds); one of $FAKE_RACING is active when listed but no longer when
// its start is read; one of $FAKE_AT_EMPTY has no start time; one of $FAKE_STOPPED is
// not running (not listed); one of $FAKE_MUTE cannot even say its version;
// $FAKE_RESTART makes restarts fail; $FAKE_SHOW_FAILS makes the start times unreadable.
// $FAKE_INSTALL makes install.sh fail before it replaces anything ("before"), or after,
// its mark naming this run ("after"). $FAKE_ROOT_FS, $FAKE_DATA_FS, $FAKE_TIMESYNC_FS,
// $FAKE_TIMESYNCD and $FAKE_MODE are what /, /data and timesyncd's folder are mounted
// as, timesyncd's state and the alarm's mode; $FAKE_NO_CLOUD, an agent that never
// reached the cloud.
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
"findmnt -no FSTYPE /") echo "${FAKE_ROOT_FS:-ext4}" ;;
"findmnt -no FSTYPE /data") echo "${FAKE_DATA_FS:-ext4}" ;;
"findmnt -no FSTYPE /var/lib/systemd/timesync") echo "${FAKE_TIMESYNC_FS:-ext4}" ;;
*"is-active systemd-timesyncd"*) echo "${FAKE_TIMESYNCD:-active}" ;;
*engine.json*) echo "${FAKE_MODE:-armed_home}" ;;
*journalctl*)
	if [ -n "$FAKE_NO_CLOUD" ]; then exit 1; fi
	echo "connected to the cloud" ;;
*nameserver*) echo 1 ;;
*NTPSynchronized*) echo yes ;;
"date +%s") date +%s ;;
*list-units*)
	if among "$FAKE_CRASHING"; then
		echo "safehome-hub.service loaded activating auto-restart safeHome hub"
	elif ! among "$FAKE_STOPPED"; then
		echo "safehome-hub.service loaded active running safeHome hub"
	fi
	echo "safehome-agent.service loaded active running safeHome agent"
	echo "mosquitto.service loaded active running Mosquitto" ;;
*ActiveEnterTimestampMonotonic*)
	if [ -n "$FAKE_SHOW_FAILS" ]; then exit 1; fi
	state=active at=100000000
	if among "$FAKE_CRASHING" || among "$FAKE_RACING"; then state=activating; fi
	if among "$FAKE_FLAPPING"; then
		at=999500000
		if [ -n "$FAKE_FLAP_AGE" ]; then at=$(( (1000 - FAKE_FLAP_AGE) * 1000000 )); fi
	fi
	if among "$FAKE_AT_EMPTY"; then at=; fi
	printf '1000.00 3000.00\n\n'
	printf 'Id=safehome-hub.service\nActiveState=%s\nActiveEnterTimestampMonotonic=%s\n\n' "$state" "$at"
	printf 'ActiveEnterTimestampMonotonic=100000000\nActiveState=active\nId=safehome-agent.service\n\n'
	printf 'Id=mosquitto.service\nActiveEnterTimestampMonotonic=90000000\nActiveState=active\n' ;;
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
	const (
		kept    = "kept"    // nothing put back
		back    = "back"    // every .prev put back
		partial = "partial" // the hub back, the gateway's .prev stuck
	)
	for _, c := range []struct {
		name, installed, crashing, flapping, flapAge, stopped, mute, install, mvFails, restart string
		noPrev, marked                                                                         bool
		state                                                                                  string
		want                                                                                   []string
	}{
		{name: "the new hub crashes at start", installed: "0.3.0", crashing: "0.3.0", marked: true, state: back,
			want: []string{"the new programs do not hold; the previous programs are back and their services hold"}},
		{name: "the new hub crashes a moment after it is ready", installed: "0.3.0", flapping: "0.3.0", marked: true, state: back,
			want: []string{"the new programs do not hold; the previous programs are back and their services hold"}},
		// After install.sh's blocking restarts a unit that held is two minutes old: 100 s.
		{name: "the new hub restarted 90 s ago", installed: "0.3.0", flapping: "0.3.0", flapAge: "90", marked: true, state: back,
			want: []string{"!! not active for the last 100 s: safehome-hub.service (active, since 90 s)", "the new programs do not hold; the previous programs are back and their services hold"}},
		{name: "the new hub restarted 99 s ago", installed: "0.3.0", flapping: "0.3.0", flapAge: "99", marked: true, state: back,
			want: []string{"!! not active for the last 100 s", "the new programs do not hold; the previous programs are back and their services hold"}},
		{name: "the new hub restarted 101 s ago", installed: "0.3.0", flapping: "0.3.0", flapAge: "101", marked: true, state: kept,
			want: []string{"phase 2 done"}},
		{name: "the new hub is not running", installed: "0.3.0", stopped: "0.3.0", marked: true, state: back,
			want: []string{"!! safehome-hub.service is not running", "the new programs do not hold; the previous programs are back"}},
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
			bundle := filepath.Join(dir, "bundle")
			writeScript(t, filepath.Join(bundle, "deploy", "install.sh"), "exit 0")
			bin := filepath.Join(dir, "hub", "usr", "local", "bin")
			files := map[string]string{"safehome-hub": c.installed, "safehome-gsm": "new gsm", "safehome-agent": "agent"}
			if !c.noPrev {
				files["safehome-hub.prev"], files["safehome-gsm.prev"] = "0.2.1", "old gsm"
			}
			files["safehome-video.prev.new"] = "a copy cut short" // never a way back
			if c.marked {
				files[strings.TrimPrefix(unconfirmed, "usr/local/bin/")] = "run cut short"
			}
			cmd, log := updateHub(t, dir, files, "-v", "0.3.0", "2", bundle)
			cmd.Env = append(cmd.Env, "FAKE_CRASHING="+c.crashing, "FAKE_FLAPPING="+c.flapping, "FAKE_FLAP_AGE="+c.flapAge, "FAKE_STOPPED="+c.stopped,
				"FAKE_MUTE="+c.mute, "FAKE_INSTALL="+c.install, "FAKE_RESTART="+c.restart, "FAKE_MV_FAILS="+c.mvFails)
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
			_, err = os.Stat(filepath.Join(dir, "hub", unconfirmed))
			if pending := err == nil; pending != (c.marked && (c.state == partial || (c.state == kept && !holds))) {
				t.Errorf("mark %s left: %v", unconfirmed, pending)
			}
		})
	}
}

// updateHub prepares update-hub.sh against fakeHub in dir, on a hub holding files in
// /usr/local/bin, and returns the command with args and the log of the commands it ran.
func updateHub(t *testing.T, dir string, files map[string]string, args ...string) (*exec.Cmd, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	hub, log, stubs := filepath.Join(dir, "hub"), filepath.Join(dir, "log"), filepath.Join(dir, "stubs")
	writeScript(t, filepath.Join(stubs, "ssh"), fakeHub)
	writeScript(t, filepath.Join(stubs, "scp"), "exit 0")
	writeScript(t, filepath.Join(stubs, "sleep"), "exit 0")
	writeScript(t, filepath.Join(stubs, "sudo"), `[ "$1" != -n ] || shift; exec "$@"`)
	writeScript(t, filepath.Join(stubs, "mv"), `if [ -n "$FAKE_MV_FAILS" ] && [ "$2" = "$FAKE_MV_FAILS" ]; then exit 1; fi; exec /bin/mv "$@"`)
	for name, content := range files {
		writeFile(t, filepath.Join(hub, "usr", "local", "bin", name), content)
	}
	cmd := exec.Command("bash", append([]string{"update-hub.sh", "-H", "admin@rig", "-k", filepath.Join(dir, "key"),
		"-w", filepath.Join(dir, "work")}, args...)...)
	cmd.Env = append(os.Environ(), "LOG="+log, "HUB="+hub, "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	return cmd, log
}

// Phase 4 of update-hub.sh, run against the fake hub, stops at the first surprise with
// an error and says it. A hub whose clock would start from the day the system went
// read-only (timesyncd's folder not bound on the data partition, timesyncd stopped) is
// refused before the services are even waited for; nothing is put back or disarmed.
func TestUpdateHubPhaseFourChecksTheHubBackInReadOnlyMode(t *testing.T) {
	for _, c := range []struct {
		name   string
		env    []string
		want   string
		waited bool // the services were waited for
	}{
		{"the hub holds", nil, "phase 4 done", true},
		{"/ is not an overlay", []string{"FAKE_ROOT_FS=ext4"}, "/ is not an overlay", false},
		{"timesyncd's folder is not on the data partition", []string{"FAKE_TIMESYNC_FS=overlay"},
			"timesyncd's folder is not bound on /data", false},
		{"/data is not ext4", []string{"FAKE_DATA_FS=overlay"}, "/data is not ext4", false},
		{"timesyncd is not running", []string{"FAKE_TIMESYNCD=inactive"}, "systemd-timesyncd is not active", false},
		{"the hub crashes a moment after it is ready", []string{"FAKE_FLAPPING=0.3.0"}, "the services do not hold", true},
		// After a boot the start is not timed: the first minute is left to it.
		{"the hub restarted 59 s ago", []string{"FAKE_FLAPPING=0.3.0", "FAKE_FLAP_AGE=59"},
			"!! not active for the last 60 s: safehome-hub.service (active, since 59 s)", true},
		{"the hub restarted 61 s ago", []string{"FAKE_FLAPPING=0.3.0", "FAKE_FLAP_AGE=61"}, "phase 4 done", true},
		{"the hub restarted 70 s ago", []string{"FAKE_FLAPPING=0.3.0", "FAKE_FLAP_AGE=70"}, "phase 4 done", true},
		{"the hub is active when listed, no longer when its start is read", []string{"FAKE_RACING=0.3.0"},
			"!! not active for the last 60 s: safehome-hub.service (activating", true},
		{"the hub has no start time", []string{"FAKE_AT_EMPTY=0.3.0"},
			"!! not active for the last 60 s: safehome-hub.service (active", true},
		{"the start times cannot be read", []string{"FAKE_SHOW_FAILS=1"}, "!! the services' start times cannot be read", true},
		{"the alarm mode changed", []string{"FAKE_MODE=disarmed"}, "the alarm mode changed", true},
		{"the agent has not reached the cloud", []string{"FAKE_NO_CLOUD=1"}, "the agent has not reached the cloud", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "work", "mode"), "armed_home\n")
			cmd, log := updateHub(t, dir, map[string]string{"safehome-hub": "0.3.0"}, "4")
			cmd.Env = append(append(cmd.Env, "FAKE_ROOT_FS=overlay"), c.env...)
			b, err := cmd.CombinedOutput()
			out := string(b)
			holds := c.want == "phase 4 done"
			if (err == nil) != holds {
				t.Fatalf("err = %v, want an error: %v, in:\n%s", err, !holds, out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("%q not in:\n%s", c.want, out)
			}
			cmds := string(must(os.ReadFile(log)))
			if waited := strings.Contains(cmds, "list-units"); waited != c.waited {
				t.Errorf("services waited for: %v, want %v", waited, c.waited)
			}
			for _, bad := range []string{".prev", "disarm", "systemctl restart"} {
				if strings.Contains(cmds, bad) {
					t.Errorf("ran a command with %q: phase 4 only checks", bad)
				}
			}
		})
	}
}
