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
// its mark naming this run ("after"); after it, the mark's reading fails $FAKE_MARK_FAILS
// times (a connection lost). $FAKE_MARK is how the cat stub reads the mark: only through
// sudo ("0600"), never ("unreadable"), or not once install.sh ran ("unreadable after
// install"). $FAKE_ROOT_FS, $FAKE_DATA_FS, $FAKE_TIMESYNC_FS,
// $FAKE_TIMESYNCD and $FAKE_MODE are what /, /data and timesyncd's folder are mounted
// as, timesyncd's state and the alarm's mode; $FAKE_NO_CLOUD, an agent that never
// reached the cloud. The units' archive really is made, of $HUB/etc/systemd/system, in
// $HUB/data/maintenance; $FAKE_TAR makes tar fail making it ("fails"), reading it back
// ("unreadable") or extracting it ("unreadable at rollback"). install.sh writes
// $FAKE_NEW_UNIT as safehome-hub.service, writes $FAKE_CHANGED_AS in each unit of
// $FAKE_CHANGED_UNITS, and adds $FAKE_EXTRA_UNIT, enabled; the hub restarts in a loop
// while one unit says $FAKE_TIGHT. safehome-boot-clock.service is a oneshot, every other
// unit a daemon. The bundle scp copies lands in $HUB/home, where its sums are computed; /boot/firmware is $HUB/boot/firmware. The
// boot id changes once the hub is asked to reboot, unless $FAKE_NO_REBOOT; it cannot be
// read when $FAKE_NO_BOOTID is "fails", reads empty when it is "empty"; its uptime is
// always short.
const fakeHub = `for cmd; do :; done
printf '%s\n' "$cmd" >> "$LOG"
hub=$(cat "$HUB/usr/local/bin/safehome-hub")
among() { case " $1 " in *" $hub "*) return 0 ;; esac; return 1; }
units="$HUB/etc/systemd/system"
crashing() { among "$FAKE_CRASHING" || { [ -n "$FAKE_TIGHT" ] && grep -qsxF "$FAKE_TIGHT" "$units"/*; }; }
case "$cmd" in
*"-e /usr/local/bin/.safehome-prev.unconfirmed"*)
	n=$(cat "$HUB/mark-fails" 2>/dev/null || echo 0)
	if [ "$n" -gt 0 ]; then echo $((n - 1)) > "$HUB/mark-fails"; exit 255; fi ;;
esac
case "$cmd" in
*"safehome-hub version")
	if among "$FAKE_MUTE"; then exit 126; fi
	echo "safehome-hub $hub" ;;
*install.sh)
	touch "$HUB/installed"
	if [ -n "$FAKE_NEW_UNIT" ]; then printf '%s\n' "$FAKE_NEW_UNIT" > "$units/safehome-hub.service"; fi
	for u in $FAKE_CHANGED_UNITS; do printf '%s\n' "$FAKE_CHANGED_AS" > "$units/$u"; done
	if [ -n "$FAKE_EXTRA_UNIT" ]; then
		echo "[Unit] new" > "$units/$FAKE_EXTRA_UNIT"
		mkdir -p "$units/multi-user.target.wants" && ln -sf "../$FAKE_EXTRA_UNIT" "$units/multi-user.target.wants/$FAKE_EXTRA_UNIT"
	fi
	echo "${FAKE_MARK_FAILS:-0}" > "$HUB/mark-fails"
	if [ "$FAKE_INSTALL" = after ]; then echo "run $$" > "$HUB/usr/local/bin/.safehome-prev.unconfirmed"; fi
	[ -z "$FAKE_INSTALL" ] ;;
*"systemctl restart"*) [ -z "$FAKE_RESTART" ] ;;
*/usr/local/bin*) bash -c "$(printf '%s' "$cmd" | sed "s#/usr/local/bin#$HUB/usr/local/bin#g")" ;;
*/data/maintenance*) bash -c "$(printf '%s' "$cmd" | sed -e "s#/data/maintenance#$HUB/data/maintenance#g" -e "s#cd / #cd $HUB/ #")" ;;
*"cd ~/safehome"*) HOME="$HUB/home" bash -c "$cmd" ;;
*boot_id*)
	if [ "$FAKE_NO_BOOTID" = fails ]; then exit 1; fi
	if [ "$FAKE_NO_BOOTID" = empty ]; then exit 0; fi
	if [ -e "$HUB/rebooted" ]; then echo new-boot; else echo old-boot; fi ;;
"sudo -n reboot") [ -n "$FAKE_NO_REBOOT" ] || touch "$HUB/rebooted" ;;
*/proc/uptime) echo 100 ;;
*"sed -i 's/overlayroot"*)
	f="$HUB/boot/firmware/cmdline.txt"
	sed 's/overlayroot=tmpfs[^ ]* //' "$f" > "$f.sed" && /bin/mv "$f.sed" "$f" ;;
*/boot/firmware*) bash -c "$(printf '%s' "$cmd" | sed "s#/boot/firmware#$HUB/boot/firmware#g")" ;;
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
	if crashing; then
		echo "safehome-hub.service loaded activating auto-restart safeHome hub"
	elif ! among "$FAKE_STOPPED"; then
		echo "safehome-hub.service loaded active running safeHome hub"
	fi
	echo "safehome-agent.service loaded active running safeHome agent"
	echo "mosquitto.service loaded active running Mosquitto" ;;
*ActiveEnterTimestampMonotonic*)
	if [ -n "$FAKE_SHOW_FAILS" ]; then exit 1; fi
	state=active at=100000000
	if crashing || among "$FAKE_RACING"; then state=activating; fi
	if among "$FAKE_FLAPPING"; then
		at=999500000
		if [ -n "$FAKE_FLAP_AGE" ]; then at=$(( (1000 - FAKE_FLAP_AGE) * 1000000 )); fi
	fi
	if among "$FAKE_AT_EMPTY"; then at=; fi
	printf '1000.00 3000.00\n\n'
	printf 'Id=safehome-hub.service\nActiveState=%s\nActiveEnterTimestampMonotonic=%s\n\n' "$state" "$at"
	printf 'ActiveEnterTimestampMonotonic=100000000\nActiveState=active\nId=safehome-agent.service\n\n'
	printf 'Id=mosquitto.service\nActiveEnterTimestampMonotonic=90000000\nActiveState=active\n' ;;
*"is-enabled systemd-remount-fs"*) echo masked ;;
*"is-enabled safehome-hub.service"* | *"is-enabled safehome-agent.service"*) echo enabled ;;
*"is-enabled zigbee2mqtt.service"* | *"is-enabled safehome-boot-clock.service"*) echo enabled ;;
*is-enabled*) echo disabled ;;
*"-p Type --value safehome-boot-clock.service"*) echo oneshot ;;
*"-p Type --value"*) echo notify ;;
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
		name, installed, crashing, flapping, flapAge, stopped, mute, install, mvFails, restart, markFails, markMode string
		noPrev, marked                                                                                              bool
		state                                                                                                       string
		want                                                                                                        []string
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
		// The rollback's restarts block too: 100 s there as well.
		{name: "the previous hub restarted 90 s ago", installed: "0.3.0", crashing: "0.3.0", flapping: "0.2.1", flapAge: "90", marked: true, state: back,
			want: []string{"!! not active for the last 100 s", "the new programs do not hold; the previous programs are back but the services still do not hold"}},
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
		// A connection lost while the mark is read again does not pass for "nothing replaced".
		{name: "the mark cannot be read at once after install.sh replaced the programs", installed: "0.3.0", install: "after", markFails: "5", state: back,
			want: []string{"install.sh failed after replacing the programs; the previous programs are back and their services hold"}},
		{name: "the hub no longer answers after install.sh failed", installed: "0.3.0", install: "after", markFails: "6", state: kept,
			want: []string{"install.sh failed and the hub no longer answers: whether it replaced the programs is unknown, nothing was put back"}},
		// Only root reads the mark: read through sudo, never as an empty token on both sides.
		{name: "install.sh fails after replacing the programs, a mark only root can read", installed: "0.3.0", install: "after", marked: true, markMode: "0600", state: back,
			want: []string{"install.sh failed after replacing the programs; the previous programs are back and their services hold"}},
		{name: "the new programs hold, a mark only root can read", installed: "0.3.0", marked: true, markMode: "0600", state: kept,
			want: []string{"phase 2 done"}},
		{name: "the mark cannot be read before install.sh", installed: "0.3.0", marked: true, markMode: "unreadable", state: kept,
			want: []string{"the mark of an unconfirmed install cannot be read"}},
		{name: "the mark cannot be read after install.sh failed", installed: "0.3.0", install: "after", markMode: "unreadable after install", state: back,
			want: []string{"install.sh failed and its mark cannot be read; the previous programs are back and their services hold"}},
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
				"FAKE_MUTE="+c.mute, "FAKE_INSTALL="+c.install, "FAKE_RESTART="+c.restart, "FAKE_MV_FAILS="+c.mvFails,
				"FAKE_MARK_FAILS="+c.markFails, "FAKE_MARK="+c.markMode)
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
			ran := slices.ContainsFunc(cmds, func(l string) bool { return strings.HasSuffix(l, "install.sh") })
			if ran != (c.markMode != "unreadable") {
				t.Errorf("install.sh run: %v, want it run only once the mark is read", ran)
			}
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
			left := c.marked || c.install == "after"
			if pending := err == nil; pending != (left && (c.state == partial || (c.state == kept && !holds))) {
				t.Errorf("mark %s left: %v", unconfirmed, pending)
			}
		})
	}
}

// Phase 2 of update-hub.sh puts the units back too when it rolls back: those saved before
// install.sh by this run (never an older archive) replace the new ones, a unit the archive
// did not hold is removed with its link and stopped, systemd reloads, then the services
// restart. A unit alone changed, the programs the same, is rolled back as well, even when
// install.sh fails on it. An archive that cannot be read then is said, naming it.
func TestUpdateHubPhaseTwoPutsTheSavedUnitsBack(t *testing.T) {
	const tight = "[Unit] ProtectSystem=strict"
	for _, c := range []struct {
		name, newUnit, extra, tar, install, hubUnit string
		changed, changedAs                          string
		noPrev, extraLeft, hubStays                 bool
		want                                        []string
	}{
		{name: "a new unit makes the hub fail", newUnit: tight, hubUnit: "[Unit]",
			want: []string{"   unit safehome-hub.service put back as it was",
				"the new programs do not hold; the previous programs and units are back and their services hold"}},
		{name: "a new unit alone, the programs the same, makes the hub fail", newUnit: tight, noPrev: true, hubUnit: "[Unit]",
			want: []string{"the new programs do not hold; the previous units are back and their services hold"}},
		{name: "install.sh fails on a new unit, the programs the same", newUnit: tight, noPrev: true, install: "before", hubUnit: "[Unit]",
			want: []string{"install.sh failed; the previous units are back and their services hold"}},
		{name: "a unit in excess is removed", newUnit: tight, extra: "safehome-extra.service", hubUnit: "[Unit]",
			want: []string{"   unit safehome-extra.service removed: it was not there before",
				"the previous programs and units are back and their services hold"}},
		{name: "the archive cannot be read at the rollback", newUnit: tight, tar: "unreadable at rollback", hubUnit: tight, extraLeft: true,
			extra: "safehome-extra.service",
			want: []string{"!! the units could not all be put back from /data/maintenance/units-",
				"the new programs do not hold; the previous programs are back but the services still do not hold; the units could not all be put back from /data/maintenance/units-"}},
		{name: "units other than the hub's that differ come back, only the daemons restart", newUnit: tight, hubUnit: "[Unit]",
			changed: "zigbee2mqtt.service safehome-boot-clock.service", changedAs: "[Unit] changed",
			want: []string{"   unit zigbee2mqtt.service put back as it was", "   unit safehome-boot-clock.service put back as it was",
				"   safehome-boot-clock back, not restarted: its type is oneshot, not a daemon's",
				"the previous programs and units are back and their services hold"}},
		{name: "a new unit other than the hub's alone makes the services fail", noPrev: true, hubUnit: "[Unit]", hubStays: true,
			changed: "zigbee2mqtt.service", changedAs: tight,
			want: []string{"   unit zigbee2mqtt.service put back as it was",
				"the new programs do not hold; the previous units are back and their services hold"}},
		{name: "a new unit that holds stays", newUnit: "[Unit] new", extra: "safehome-extra.service", hubUnit: "[Unit] new", extraLeft: true,
			want: []string{"phase 2 done"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			bundle := filepath.Join(dir, "bundle")
			writeScript(t, filepath.Join(bundle, "deploy", "install.sh"), "exit 0")
			files := map[string]string{"safehome-hub": "0.3.0"}
			if !c.noPrev {
				files["safehome-hub.prev"] = "0.2.1"
			}
			cmd, log := updateHub(t, dir, files, "-v", "0.3.0", "2", bundle)
			units := filepath.Join(dir, "hub", "etc", "systemd", "system")
			// An older run's archive, never the way back for this one.
			old := filepath.Join(dir, "old")
			writeFile(t, filepath.Join(old, "etc", "systemd", "system", "safehome-hub.service"), "[Unit] old")
			writeFile(t, filepath.Join(dir, "hub", "data", "maintenance", ".keep"), "")
			if b, err := exec.Command("tar", "-czf", filepath.Join(dir, "hub", "data", "maintenance", "units-20260101-0000.tgz"),
				"-C", old, "etc/systemd/system/safehome-hub.service").CombinedOutput(); err != nil {
				t.Fatalf("old archive: %v %s", err, b)
			}
			wants := filepath.Join(units, "multi-user.target.wants")
			if err := os.MkdirAll(wants, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../safehome-hub.service", filepath.Join(wants, "safehome-hub.service")); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(units, "safehome-boot-clock.service"), "[Unit]")
			cmd.Env = append(cmd.Env, "FAKE_NEW_UNIT="+c.newUnit, "FAKE_TIGHT="+tight, "FAKE_EXTRA_UNIT="+c.extra,
				"FAKE_TAR="+c.tar, "FAKE_INSTALL="+c.install, "FAKE_CHANGED_UNITS="+c.changed, "FAKE_CHANGED_AS="+c.changedAs)
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
			if got := strings.TrimSpace(string(must(os.ReadFile(filepath.Join(units, "safehome-hub.service"))))); got != c.hubUnit {
				t.Errorf("safehome-hub.service = %q, want %q", got, c.hubUnit)
			}
			for _, unit := range []string{"safehome-agent.service", "zigbee2mqtt.service", "safehome-boot-clock.service"} {
				if got := strings.TrimSpace(string(must(os.ReadFile(filepath.Join(units, unit))))); got != "[Unit]" {
					t.Errorf("%s = %q, want it as it was", unit, got)
				}
			}
			if _, err := os.Lstat(filepath.Join(wants, "safehome-hub.service")); err != nil {
				t.Errorf("the hub's link in multi-user.target.wants is gone: %v", err)
			}
			if c.extra != "" {
				_, unitErr := os.Stat(filepath.Join(units, c.extra))
				_, linkErr := os.Lstat(filepath.Join(wants, c.extra))
				if left := unitErr == nil; left != c.extraLeft || (linkErr == nil) != c.extraLeft {
					t.Errorf("%s left: unit %v, link %v, want %v", c.extra, left, linkErr == nil, c.extraLeft)
				}
			}
			cmds := strings.Split(string(must(os.ReadFile(log))), "\n")
			install := slices.IndexFunc(cmds, func(l string) bool { return strings.HasSuffix(l, "install.sh") })
			restore := slices.IndexFunc(cmds, func(l string) bool { return strings.Contains(l, "tar -xzf") })
			reload := slices.Index(cmds, "sudo -n systemctl daemon-reload")
			stop := slices.Index(cmds, "sudo -n systemctl stop "+c.extra)
			// Each service whose program or unit came back restarts once; an identical
			// unit, or one that is not a daemon (safehome-boot-clock), never.
			restarted := map[string]int{}
			for _, unit := range []string{"safehome-hub", "safehome-agent", "zigbee2mqtt", "safehome-boot-clock"} {
				want := 0
				if !holds && ((unit == "safehome-hub" && !c.hubStays) ||
					(unit == "zigbee2mqtt" && strings.Contains(c.changed, "zigbee2mqtt.service"))) {
					want = 1
				}
				line := "sudo -n systemctl restart " + unit + ".service"
				if got := strings.Count("\n"+strings.Join(cmds, "\n")+"\n", "\n"+line+"\n"); got != want {
					t.Errorf("%s restarted %d times, want %d", unit, got, want)
				}
				if want == 1 {
					restarted[unit] = slices.Index(cmds, line)
				}
			}
			restart := -1
			for _, i := range restarted {
				if restart < 0 || i < restart {
					restart = i
				}
			}
			if holds {
				if restore >= 0 || reload >= 0 {
					t.Errorf("units put back at %d, daemon-reload at %d: the healthy path puts nothing back", restore, reload)
				}
			} else if restore < install || reload < restore || restart < reload {
				t.Errorf("install.sh at %d, units put back at %d, daemon-reload at %d, first restart at %d: want them in this order",
					install, restore, reload, restart)
			}
			if wantStop := c.extra != "" && !c.extraLeft; (stop >= 0) != wantStop || (wantStop && stop < reload) {
				t.Errorf("%s stopped at %d, daemon-reload at %d: want it stopped after, only once removed", c.extra, stop, reload)
			}
			if !strings.Contains(strings.Join(cmds, "\n"), "units-2026") || strings.Contains(strings.Join(cmds, "\n"), "units-20260101-0000") {
				t.Error("the units came from an older archive than this run's")
			}
			for _, l := range cmds {
				if strings.Contains(l, "engine.json") || strings.Contains(l, "disarm") {
					t.Errorf("ran %q: the alarm is left alone", l)
				}
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
	// scp copies the bundle into the hub's home folder; $FAKE_ALTER is a file changed on
	// the way, $FAKE_DROP one lost.
	writeScript(t, filepath.Join(stubs, "scp"), `src= dst=
for a; do src=$dst; dst=$a; done
mkdir -p "$HUB/home" && cp -R "$src" "$HUB/home/${dst#*:}"
if [ -n "$FAKE_ALTER" ]; then echo altered >> "$HUB/home/safehome/$FAKE_ALTER"; fi
if [ -n "$FAKE_DROP" ]; then rm "$HUB/home/safehome/$FAKE_DROP"; fi`)
	writeScript(t, filepath.Join(stubs, "tar"), `case " $* " in
*" -czf "*) if [ "$FAKE_TAR" = fails ]; then echo "tar: write error" >&2; exit 2; fi ;;
*" -tzf "*) if [ "$FAKE_TAR" = unreadable ]; then echo "gzip: stdin: unexpected end of file" >&2; exit 2; fi ;;
*" -xzf "*) if [ "$FAKE_TAR" = "unreadable at rollback" ]; then echo "gzip: stdin: unexpected end of file" >&2; exit 2; fi ;;
esac
exec /usr/bin/tar "$@"`)
	for _, unit := range []string{"safehome-hub.service", "safehome-agent.service", "zigbee2mqtt.service"} {
		writeFile(t, filepath.Join(hub, "etc", "systemd", "system", unit), "[Unit]")
	}
	// $FAKE_CUT: the power is cut while tee writes, its file left half written;
	// $FAKE_GARBLE: tee says it wrote, but its file is not what it was given.
	writeScript(t, filepath.Join(stubs, "tee"), `if [ -n "$FAKE_CUT" ]; then for a; do :; done; head -c 12 > "$a"; exit 1; fi
if [ -n "$FAKE_GARBLE" ]; then for a; do :; done; { /bin/cat; echo garbled; } > "$a"; exit 0; fi
exec /usr/bin/tee "$@"`)
	writeScript(t, filepath.Join(stubs, "sleep"), "exit 0")
	writeScript(t, filepath.Join(stubs, "sudo"), `[ "$1" != -n ] || shift; SUDO=1 exec "$@"`)
	writeScript(t, filepath.Join(stubs, "cat"), `for a; do :; done
case "$a" in *.safehome-prev.unconfirmed)
	if [ "$FAKE_MARK" = unreadable ] || { [ "$FAKE_MARK" = "unreadable after install" ] && [ -e "$HUB/installed" ]; } ||
		{ [ "$FAKE_MARK" = 0600 ] && [ -z "$SUDO" ]; }; then echo "cat: $a: Permission denied" >&2; exit 1; fi ;;
esac
exec /bin/cat "$@"`)
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

// Phase 2 of update-hub.sh installs nothing from a bundle that is not, file for file,
// the one made here (sums computed here before scp, on the hub after), nor before the
// units are saved and their archive read back: it stops before install.sh, every
// program left as it is.
func TestUpdateHubPhaseTwoInstallsNothingUncheckedOrWithoutTheUnitsSaved(t *testing.T) {
	for _, c := range []struct {
		name, want string
		env        []string
		copied     bool // the bundle was copied and its sums computed on the hub
		noHubUnit  bool // safehome-hub.service is not in /etc/systemd/system
	}{
		{"the bundle arrives whole", "phase 2 done", nil, true, false},
		{"a file is changed on the way", "the bundle on the hub differs from this one (./deploy/install.sh ): nothing installed",
			[]string{"FAKE_ALTER=deploy/install.sh"}, true, false},
		{"a file is lost on the way", "the bundle on the hub differs from this one (./config/hub.example.json ): nothing installed",
			[]string{"FAKE_DROP=config/hub.example.json"}, true, false},
		{"the units cannot be saved", "the units could not be saved in /data/maintenance/units-", []string{"FAKE_TAR=fails"}, false, false},
		{"the units' archive cannot be read back", ".tgz cannot be read back: nothing installed", []string{"FAKE_TAR=unreadable"}, false, false},
		{"the units' archive lacks the hub's unit", ".tgz does not hold safehome-hub.service: nothing installed", nil, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			bundle := filepath.Join(dir, "bundle")
			writeScript(t, filepath.Join(bundle, "deploy", "install.sh"), "exit 0")
			writeFile(t, filepath.Join(bundle, "config", "hub.example.json"), "{}")
			files := map[string]string{"safehome-hub": "0.3.0", "safehome-hub.prev": "0.2.1"}
			cmd, log := updateHub(t, dir, files, "-v", "0.3.0", "2", bundle)
			cmd.Env = append(cmd.Env, c.env...)
			if c.noHubUnit {
				if err := os.Remove(filepath.Join(dir, "hub", "etc", "systemd", "system", "safehome-hub.service")); err != nil {
					t.Fatal(err)
				}
			}
			b, err := cmd.CombinedOutput()
			out := string(b)
			holds := c.want == "phase 2 done"
			if (err == nil) != holds {
				t.Fatalf("err = %v, want an error: %v, in:\n%s", err, !holds, out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("%q not in:\n%s", c.want, out)
			}
			cmds := strings.Split(string(must(os.ReadFile(log))), "\n")
			saved := slices.IndexFunc(cmds, func(l string) bool { return strings.Contains(l, "tar -czf") })
			read := slices.IndexFunc(cmds, func(l string) bool { return strings.Contains(l, "tar -tzf") })
			sums := slices.IndexFunc(cmds, func(l string) bool { return strings.Contains(l, "cd ~/safehome") })
			install := slices.IndexFunc(cmds, func(l string) bool { return strings.HasSuffix(l, "install.sh") })
			if (install >= 0) != holds {
				t.Errorf("install.sh run: %v, want it run only from a checked bundle, the units saved", install >= 0)
			}
			if (sums >= 0) != c.copied {
				t.Errorf("sums computed on the hub: %v, want %v", sums >= 0, c.copied)
			}
			if saved < 0 || (c.copied && (read < saved || sums < read)) || (holds && install < sums) {
				t.Errorf("units saved at %d, read back at %d, sums at %d, install.sh at %d: want them in this order", saved, read, sums, install)
			}
			for _, l := range cmds {
				if strings.Contains(l, ".prev") || strings.Contains(l, "systemctl restart") || strings.Contains(l, "disarm") {
					t.Errorf("ran %q: nothing is put back, restarted or disarmed", l)
				}
			}
			if archives, _ := filepath.Glob(filepath.Join(dir, "hub", "data", "maintenance", "units-*.tgz")); holds && len(archives) != 1 {
				t.Errorf("units' archives: %v, want one", archives)
			}
			for name, want := range files {
				if got := string(must(os.ReadFile(filepath.Join(dir, "hub", "usr", "local", "bin", name)))); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

// Phases 1 and 3 reboot the hub and wait for it: it is back once it answers from
// another boot (its boot id), never because its uptime is short, and a hub whose boot id
// cannot be read is not rebooted at all. Phase 3 never writes cmdline.txt in place: a
// cut while cmdline.txt.new is written, a cmdline.txt.new that is not the saved one, or
// a rename that fails leaves the one in place whole, and nothing reboots.
func TestUpdateHubRebootsForRealAndNeverWritesCmdlineInPlace(t *testing.T) {
	const (
		readOnly = "console=serial0,115200 root=PARTUUID=1-02 overlayroot=tmpfs:recurse=0 rootwait\n"
		writable = "console=serial0,115200 root=PARTUUID=1-02 rootwait\n"
	)
	for _, c := range []struct {
		name, phase, want, cmdline string
		env                        []string
	}{
		{"phase 1, the hub reboots", "1", "phase 1 done", writable, nil},
		{"phase 1, the hub does not reboot, its uptime short", "1",
			"the hub has not restarted after 4 minutes: it still answers from the same boot", writable, []string{"FAKE_NO_REBOOT=1"}},
		{"phase 3, the hub reboots", "3", "phase 3 done", readOnly, nil},
		{"phase 3, the hub does not reboot, its uptime short", "3",
			"the hub has not restarted after 4 minutes: it still answers from the same boot", readOnly, []string{"FAKE_NO_REBOOT=1"}},
		{"phase 3, a cut while cmdline.txt is written", "3",
			"cmdline.txt.new could not be written: cmdline.txt left as it was", writable, []string{"FAKE_CUT=1"}},
		{"phase 3, cmdline.txt.new is not what was sent", "3",
			"cmdline.txt.new differs from the saved one: cmdline.txt left as it was", writable, []string{"FAKE_GARBLE=1"}},
		{"phase 3, cmdline.txt.new cannot be renamed", "3",
			"cmdline.txt could not be replaced by cmdline.txt.new", writable, []string{"FAKE_MV_FAILS=cmdline.txt.new"}},
		{"phase 1, the boot id cannot be read", "1",
			"the hub's boot id cannot be read: not rebooting", writable, []string{"FAKE_NO_BOOTID=fails"}},
		{"phase 1, the boot id reads empty", "1",
			"the hub's boot id cannot be read: not rebooting", writable, []string{"FAKE_NO_BOOTID=empty"}},
		{"phase 3, the boot id cannot be read", "3",
			"the hub's boot id cannot be read: not rebooting", readOnly, []string{"FAKE_NO_BOOTID=fails"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			onHub := filepath.Join(dir, "hub", "boot", "firmware", "cmdline.txt")
			if c.phase == "1" {
				writeFile(t, onHub, readOnly)
			} else {
				writeFile(t, onHub, writable)
				writeFile(t, filepath.Join(dir, "work", "cmdline.txt"), readOnly)
			}
			cmd, log := updateHub(t, dir, map[string]string{"safehome-hub": "0.3.0"}, c.phase)
			for _, e := range c.env {
				if e == "FAKE_MV_FAILS=cmdline.txt.new" { // the mv stub compares its source, in full
					e = "FAKE_MV_FAILS=" + onHub + ".new"
				}
				cmd.Env = append(cmd.Env, e)
			}
			b, err := cmd.CombinedOutput()
			out := string(b)
			holds := c.want == "phase "+c.phase+" done"
			if (err == nil) != holds {
				t.Fatalf("err = %v, want an error: %v, in:\n%s", err, !holds, out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("%q not in:\n%s", c.want, out)
			}
			if got := string(must(os.ReadFile(onHub))); got != c.cmdline {
				t.Errorf("cmdline.txt = %q, want %q", got, c.cmdline)
			}
			if _, err := os.Stat(onHub + ".new"); holds && err == nil {
				t.Error("cmdline.txt.new left beside cmdline.txt")
			}
			cmds := strings.Split(string(must(os.ReadFile(log))), "\n")
			// Asked to reboot only once cmdline.txt is in place and the boot id read.
			rebooted := slices.Contains(cmds, "sudo -n reboot")
			if want := holds || strings.Contains(c.want, "has not restarted"); rebooted != want {
				t.Errorf("rebooted: %v, want %v", rebooted, want)
			}
			for _, l := range cmds {
				if strings.Contains(l, "tee /boot/firmware/cmdline.txt ") || (strings.Contains(l, "engine.json") && c.phase == "3") {
					t.Errorf("ran %q: cmdline.txt is never written in place, phase 3 leaves the alarm alone", l)
				}
			}
		})
	}
}
