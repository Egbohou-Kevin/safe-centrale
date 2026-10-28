#!/usr/bin/env bash
# Updates a hub in read-only mode from a bundle made by deploy/bundle.sh, without -s
# (docs/systeme.md, "Maintenance"), in four phases run one after the other:
#
#   deploy/update-hub.sh 1                     save cmdline.txt and the alarm's mode, leave read-only, reboot
#   deploy/update-hub.sh -v VERSION 2 BUNDLE   write access, DNS and clock, install, check, clean up
#   deploy/update-hub.sh 3                     back to read-only (cmdline.txt as saved), reboot
#   deploy/update-hub.sh 4                     overlay, services, same alarm mode, cloud reached
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

wait_services() {
	echo "== services for two minutes"
	sleep 120
	pi "systemctl list-units $units --no-legend --plain" | awk '{print "   " $1, $3, $4}'
	bad=$(pi "systemctl list-units $units --no-legend --plain --state=failed" | wc -l)
	[ "$bad" -eq 0 ] || die "$bad service(s) failed"
	echo "   failed units:"
	pi 'systemctl --failed --no-legend --plain' | sed 's/^/     /'
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
	echo "== installing"
	if ! pi 'sudo -n ./safehome/deploy/install.sh' 2>&1 | tail -n 40; then
		die "install.sh failed (it put the previous configuration back)"
	fi
	got=$(pi '/usr/local/bin/safehome-hub version')
	[ "$got" = "safehome-hub $version" ] || die "the hub says $got"
	echo "   $got"
	wait_services
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
	echo "== / overlay, /data ext4"
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
