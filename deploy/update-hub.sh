#!/usr/bin/env bash
# Updates a hub in read-only mode from a bundle made by deploy/bundle.sh, without -s
# (docs/systeme.md, "Maintenance"), in four phases run one after the other:
#
#   deploy/update-hub.sh 1                     save cmdline.txt and the alarm's mode, leave read-only, reboot
#   deploy/update-hub.sh -v VERSION 2 BUNDLE   write access, DNS and clock, install, check, clean up;
#                                              the previous programs back if the new ones do not hold
#   deploy/update-hub.sh 3                     back to read-only (cmdline.txt as saved), reboot
#   deploy/update-hub.sh 4                     overlay, timesyncd's clock on /data, services, same alarm mode, cloud reached
#
#   -H HOST     the hub (default admin@safe-centrale.local)
#   -k KEY      its SSH key (default ~/.ssh/safehome_ed25519)
#   -w DIR      where the saved cmdline.txt and mode are kept (default ~/.safehome-maintenance)
#   -v VERSION  the bundle's version, checked after the install (phase 2)
#
# Nothing is ever disarmed. Each phase stops at the first surprise. Phases 1 and 3
# reboot the hub: about a minute without alarm each.
set -euo pipefail

usage() {
	echo "usage: deploy/update-hub.sh [-H HOST] [-k KEY] [-w DIR] [-v VERSION] 1|2|3|4 [BUNDLE]" >&2
	exit 2
}

hub=admin@safe-centrale.local
key=~/.ssh/safehome_ed25519
work=~/.safehome-maintenance
version=""
while getopts "H:k:w:v:" opt; do
	case "$opt" in
	H) hub="$OPTARG" ;;
	k) key="$OPTARG" ;;
	w) work="$OPTARG" ;;
	v) version="$OPTARG" ;;
	*) usage ;;
	esac
done
shift $((OPTIND - 1))
[ $# -ge 1 ] || usage
phase=$1
mkdir -p "$work"

pi() { ssh -i "$key" -o BatchMode=yes -o ConnectTimeout=10 "$hub" "$@"; }
die() {
	echo "!! $*" >&2
	exit 1
}
mode() { pi 'sudo -n python3 -c "import json;print(json.load(open(\"/var/lib/safehome/engine.json\"))[\"mode\"])"'; }
units='"safehome-*" mosquitto.service zigbee2mqtt.service'

wait_back() {
	echo "== waiting for the hub to come back (4 minutes at most)"
	sleep 20
	for i in $(seq 1 44); do
		if [ "$(pi 'cut -d. -f1 /proc/uptime' 2>/dev/null || echo 999)" -lt 300 ]; then
			echo "   back after $((20 + i * 5)) s"
			return 0
		fi
		sleep 5
	done
	die "the hub did not come back after 4 minutes"
}

# services_hold SECONDS: after two minutes, safehome-hub.service running, every unit
# active for the last SECONDS at least, none failed. A program that crashes at start never
# reaches failed (Restart=always, no start limit): it stays activating, so anything but
# active counts. One that crashes a moment after it says it is ready (RestartSec=2) is
# active most of the time: its last start (ActiveEnterTimestampMonotonic, against
# /proc/uptime) must be SECONDS old. After install.sh or roll_back, each restart blocks
# until the unit is ready (Type=notify), so a unit that held is two minutes old: 100 s
# then. After a boot the start is not timed: 60 s, the first minute left to it. A unit
# that is not running at all is not listed: the hub must be.
services_hold() {
	held=$1
	echo "== services for two minutes"
	sleep 120
	# Called after ||, where set -e does not stop: each failure is said here.
	if ! list=$(pi "systemctl list-units $units --no-legend --plain"); then
		echo "!! the services cannot be listed" >&2
		return 1
	fi
	printf '%s\n' "$list" | awk 'NF {print "   " $1, $3, $4}'
	bad=$(printf '%s\n' "$list" | awk 'NF && $3 != "active" {n++} END {print n + 0}')
	if [ "$bad" -ne 0 ]; then
		echo "!! $bad service(s) not active" >&2
		return 1
	fi
	if ! printf '%s\n' "$list" | awk '$1 == "safehome-hub.service" {found = 1} END {exit !found}'; then
		echo "!! safehome-hub.service is not running" >&2
		return 1
	fi
	names=$(printf '%s\n' "$list" | awk 'NF {printf "%s ", $1}')
	if ! since=$(pi "cat /proc/uptime && echo && systemctl show -p Id -p ActiveState -p ActiveEnterTimestampMonotonic $names"); then
		echo "!! the services' start times cannot be read" >&2
		return 1
	fi
	# One block per unit, its lines in systemd's order, not the order asked.
	recent=$(printf '%s\n' "$since" | awk -v held="$held" 'BEGIN {RS = ""; FS = "\n"}
		NR == 1 {split($1, u, " "); now = u[1] * 1000000; next}
		{
			id = state = at = ""
			for (i = 1; i <= NF; i++) {
				k = substr($i, 1, index($i, "=") - 1)
				v = substr($i, index($i, "=") + 1)
				if (k == "Id") id = v
				else if (k == "ActiveState") state = v
				else if (k == "ActiveEnterTimestampMonotonic") at = v
			}
			if (state != "active" || at == "" || now - at < held * 1000000)
				printf "%s (%s, since %d s) ", id, state, (now - at) / 1000000
		}')
	if [ -n "$recent" ]; then
		echo "!! not active for the last $held s: $recent" >&2
		return 1
	fi
	echo "   failed units:"
	pi 'systemctl --failed --no-legend --plain' | sed 's/^/     /'
}

wait_services() { services_hold 60 || die "the services do not hold"; }

# The mark install.sh leaves while the programs in place are not yet confirmed; it names
# the run that made it.
unconfirmed=/usr/local/bin/.safehome-prev.unconfirmed
mark() { pi "if [ -e $unconfirmed ]; then echo \"mark \$(cat $unconfirmed)\"; else echo none; fi"; }

# roll_back: the programs install.sh replaced go back (it kept each one as X.prev,
# docs/systeme.md, "Maintenance"; an X.prev.new is a copy cut short, never put back),
# and the services of those enabled restart on them.
# Nothing is disarmed; the alarm's state (/var/lib/safehome) is not touched. Called
# last after ||, where set -e still stops: each step that may fail is said here.
roll_back() {
	echo "== putting the previous programs back"
	if ! back=$(pi "sudo -n sh -c 'cd /usr/local/bin && rc=0 && for f in safehome-*.prev; do [ -e \"\$f\" ] || continue; if mv -f \"\$f\" \"\${f%.prev}\"; then echo \"\${f%.prev}\"; else rc=1; fi; done; [ \$rc != 0 ] || rm -f $unconfirmed; exit \$rc'"); then
		die "$1, and the previous programs could not all be put back (back: $(printf '%s' "${back:-none}" | tr '\n' ' ')); the others are still safehome-*.prev in /usr/local/bin"
	fi
	[ -n "$back" ] || die "$1, and no previous program was kept: nothing to put back"
	pi 'sudo -n systemctl daemon-reload' || echo "!! systemctl daemon-reload failed" >&2
	for p in $back; do
		if [ "$(pi "systemctl is-enabled $p.service 2>/dev/null || true")" != enabled ]; then
			echo "   $p back"
		elif pi "sudo -n systemctl restart $p.service"; then
			echo "   $p back, restarted"
		else
			echo "!! $p back, but its restart failed" >&2
		fi
	done
	echo "   $(pi '/usr/local/bin/safehome-hub version' || echo 'hub version not readable')"
	services_hold 100 || die "$1; the previous programs are back but the services still do not hold"
	die "$1; the previous programs are back and their services hold"
}

# Out of read-only mode, / is still read-only at boot: NetworkManager cannot write
# /etc/resolv.conf, and without DNS the clock never syncs (28 October 2026).
dns_and_clock() {
	if [ "$(pi 'grep -c ^nameserver /etc/resolv.conf || true')" = 0 ]; then
		pi 'sudo -n cp /run/NetworkManager/resolv.conf /etc/resolv.conf && sudo -n systemctl restart systemd-timesyncd'
		echo "   resolv.conf restored from NetworkManager's"
	fi
	for i in $(seq 1 24); do
		[ "$(pi 'timedatectl show -p NTPSynchronized --value')" = yes ] && break
		[ "$i" = 24 ] && die "the hub's clock is not synchronised after 2 minutes"
		sleep 5
	done
	drift=$(($(pi 'date +%s') - $(date +%s)))
	[ "${drift#-}" -lt 60 ] || die "the hub's clock is off by $drift s"
	echo "== writable, clock right (drift $drift s)"
}

case "$phase" in
1)
	pi 'cat /boot/firmware/cmdline.txt' >"$work/cmdline.txt"
	grep -q 'overlayroot=tmpfs:recurse=0 ' "$work/cmdline.txt" || die "cmdline.txt has no overlayroot"
	mode >"$work/mode"
	echo "== saved in $work: cmdline.txt, alarm mode $(cat "$work/mode") (left as it is)"
	pi "sudo -n sed -i 's/overlayroot=tmpfs[^ ]* //' /boot/firmware/cmdline.txt"
	[ "$(pi 'grep -c overlayroot /boot/firmware/cmdline.txt || true')" = 0 ] || die "overlayroot still in cmdline.txt"
	echo "== read-only removed from cmdline.txt, rebooting"
	pi 'sudo -n reboot' || true
	wait_back
	;;
2)
	[ $# -eq 2 ] && [ -n "$version" ] || usage
	bundle=$2
	[ -x "$bundle/deploy/install.sh" ] || die "$bundle is not a bundle"
	[ ! -d "$bundle/site" ] || die "the bundle holds a site folder (secrets): make it without -s"
	[ "$(pi 'findmnt -no FSTYPE /')" = ext4 ] || die "/ is not ext4: run phase 1 first"
	pi 'sudo -n mount -o remount,rw /'
	dns_and_clock
	stamp=$(date +%Y%m%d-%H%M)
	pi "sudo -n mkdir -p /data/maintenance && sudo -n sh -c 'tar -czf /data/maintenance/units-$stamp.tgz /etc/systemd/system/safehome* /etc/systemd/system/zigbee2mqtt* 2>/dev/null; true'"
	echo "== units kept in /data/maintenance/units-$stamp.tgz, copying the bundle"
	pi 'rm -rf ~/safehome'
	scp -q -r -i "$key" "$bundle" "$hub:safehome"
	# A mark left by an earlier run cut short is not this run's: install.sh may fail
	# before replacing anything, and those programs then stay as they are.
	before=$(mark) || die "the mark of an unconfirmed install cannot be read"
	echo "== installing"
	if ! pi 'sudo -n ./safehome/deploy/install.sh' 2>&1 | tail -n 40; then
		# Past the configuration check, install.sh has replaced the programs and left
		# its mark, naming this run: a restart that failed puts the previous ones back too.
		after=$(mark || true)
		if [ "${after#mark }" != "$after" ] && [ "$after" != "$before" ]; then
			roll_back "install.sh failed after replacing the programs"
		fi
		[ "$before" = none ] ||
			die "install.sh failed before replacing the programs (it put the previous configuration back); the programs in place are still those of an earlier run, not confirmed"
		die "install.sh failed before replacing the programs (it put the previous configuration back)"
	fi
	# From here the programs are the new ones: if they do not hold, the previous ones
	# go back before stopping. A hub that cannot even say its version counts.
	got=$(pi '/usr/local/bin/safehome-hub version') || got="(version not readable)"
	[ "$got" = "safehome-hub $version" ] || roll_back "the hub says $got"
	echo "   $got"
	services_hold 100 || roll_back "the new programs do not hold"
	pi "sudo -n rm -f $unconfirmed"
	echo "== the new programs hold: the previous ones stay as .prev, no longer pending"
	pi 'rm -rf ~/safehome'
	[ -z "$(pi 'ls ~')" ] || die "the home folder is not empty: $(pi 'ls ~')"
	echo "== home folder empty"
	;;
3)
	[ -s "$work/cmdline.txt" ] || die "no saved cmdline.txt: phase 1 saves it"
	pi 'sudo -n raspi-config nonint enable_overlayfs'
	pi 'sudo -n tee /boot/firmware/cmdline.txt >/dev/null' <"$work/cmdline.txt"
	pi 'cat /boot/firmware/cmdline.txt' | cmp - "$work/cmdline.txt" || die "cmdline.txt differs from the saved one"
	[ "$(pi 'systemctl is-enabled systemd-remount-fs || true')" = masked ] || die "systemd-remount-fs is not masked"
	echo "== cmdline.txt as saved, remount-fs masked, rebooting"
	pi 'sudo -n reboot' || true
	wait_back
	;;
4)
	[ "$(pi 'findmnt -no FSTYPE /')" = overlay ] || die "/ is not an overlay"
	[ "$(pi 'findmnt -no FSTYPE /data')" = ext4 ] || die "/data is not ext4"
	[ "$(pi 'findmnt -no FSTYPE /var/lib/systemd/timesync')" = ext4 ] ||
		die "timesyncd's folder is not bound on /data: the clock at boot would start from the day the system went read-only"
	[ "$(pi 'systemctl is-active systemd-timesyncd || true')" = active ] || die "systemd-timesyncd is not active"
	echo "== / overlay, /data ext4, timesyncd active, its clock file on /data"
	wait_services
	now=$(mode)
	echo "== alarm mode: before $(cat "$work/mode"), now $now"
	[ "$now" = "$(cat "$work/mode")" ] || die "the alarm mode changed"
	pi 'sudo -n journalctl -u safehome-agent -b --no-pager -o cat | grep -m1 "connected to the cloud"' ||
		die "the agent has not reached the cloud"
	echo "   $(pi '/usr/local/bin/safehome-hub version')"
	;;
*)
	usage
	;;
esac
echo "== phase $phase done"
