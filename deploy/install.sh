#!/usr/bin/env bash
# Installs or updates safeHome on the hub from a bundle made by deploy/bundle.sh
# (docs/systeme.md). Run on the Raspberry Pi, as root, with the system writable (not
# in read-only mode) and the data partition mounted (step 2 of docs/systeme.md):
#
#   sudo ./safehome/deploy/install.sh
#
# It can be run again at each update. It replaces the programs, the deployment files
# and the device profiles; it replaces the site's configuration only with what the
# bundle's site/ folder holds, and never touches the Zigbee network. Every
# configuration is checked with the new programs before anything restarts: a bad one
# puts the previous files back and stops there.
set -euo pipefail

# Tests only: a folder that stands for / (commands are faked through PATH).
ROOT="${SAFEHOME_ROOT:-}"

# The Node.js lines Zigbee2MQTT runs on here: its LTS lines. Zigbee2MQTT 2.14.2 declares
# "^22.2.0 || ^24 || <=26.2", and with Node 26.5 the stick's serial port opened but
# nothing written reached it (trial on real hardware, 24 October 2026; docs/systeme.md,
# step 8). A version outside them stops the script before it changes anything.
NODE_MAJORS="22 24"

# The services under cmd/, and the tools run by hand (deploy_test.go checks both lists).
PROGRAMS="hub gsm zigbee agent video maint console power"
TOOLS="install"
# Every site runs these; video (cameras), maint (remote maintenance), console (the
# local console) and power (the battery module) only when their configuration is there.
# A site without a GSM line (without_gsm in hub.json) runs no gsm.
REQUIRED="hub gsm zigbee agent"

BUNDLE="$(cd "$(dirname "$0")/.." && pwd)"
ETC="$ROOT/etc/safehome"
BIN="$ROOT/usr/local/bin"
UNITS="$ROOT/etc/systemd/system"
SITE="$BUNDLE/site"

say() { printf 'install.sh: %s\n' "$*"; }
fail() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

# configured NAME FILE: the file is in the bundle's site/ folder or already installed.
configured() { [ -f "$SITE/$1" ] || [ -f "$ETC/$1" ]; }

# --- Before changing anything ---------------------------------------------------

[ "$(id -u)" = 0 ] || fail "run as root (sudo)"
[ "$(uname -m)" = aarch64 ] || fail "not a 64-bit Raspberry Pi (uname -m is $(uname -m))"
if [ "$(findmnt -n -o FSTYPE /)" = overlay ]; then
	fail "the system is read-only: sudo raspi-config nonint disable_overlayfs && sudo reboot, then run again"
fi
for d in /data /var/lib/safehome /var/lib/mosquitto /var/lib/zigbee2mqtt; do
	mountpoint -q "$ROOT$d" || fail "$d is not mounted: do step 2 of docs/systeme.md first"
done
for p in $PROGRAMS $TOOLS; do
	[ -x "$BUNDLE/bin/safehome-$p" ] || fail "the bundle has no bin/safehome-$p: make it with deploy/bundle.sh"
done
if [ -d "$ROOT/opt/zigbee2mqtt" ]; then
	# The node of zigbee2mqtt.service, whatever else is on the PATH.
	node_version="$("$ROOT/usr/bin/node" --version 2>/dev/null)" ||
		fail "Zigbee2MQTT is installed but not /usr/bin/node: install Node.js 24 as step 8 of docs/systeme.md says"
	node_major="${node_version#v}"
	node_major="${node_major%%.*}"
	case " $NODE_MAJORS " in
	*" $node_major "*) ;;
	*) fail "Node.js $node_version is not one Zigbee2MQTT runs on here (majors: $NODE_MAJORS); with 26.5 the Zigbee stick never answered: install Node.js 24 as step 8 of docs/systeme.md says" ;;
	esac
fi

site_file() { if [ -f "$SITE/$1" ]; then echo "$SITE/$1"; else echo "$ETC/$1"; fi; }
# A site without a GSM line, by choice ("without_gsm": true in hub.json): no gsm.json,
# no gateway (docs/gsm-gateway.md).
required="$REQUIRED"
if configured hub.json && grep -Eq '"without_gsm"[[:space:]]*:[[:space:]]*true' "$(site_file hub.json)"; then
	configured gsm.json &&
		fail "hub.json says without_gsm but gsm.json is there: remove it (and /etc/safehome/gsm.json), or drop without_gsm"
	required="${required/ gsm / }"
fi

enabled=""
missing=""
for p in $PROGRAMS; do
	if configured "$p.json"; then
		enabled="$enabled $p"
	else
		case " $required " in *" $p "*) missing="$missing $p.json" ;; esac
	fi
done
configured cloud.secret || missing="$missing cloud.secret"
[ -z "$missing" ] || fail "no$missing: put them in the bundle's site/ folder (start from config/*.example.json)"

has() { case " $enabled " in *" $1 "*) return 0 ;; esac; return 1; }
if has video && [ ! -x "$BUNDLE/bin/go2rtc" ] && [ ! -x "$BIN/go2rtc" ]; then
	fail "video.json is there but not go2rtc: give it to deploy/bundle.sh (-g)"
fi
# json_value KEY FILE: a string value of a configuration file (enough for an id).
json_value() { sed -n 's/.*"'"$1"'": *"\([^"]*\)".*/\1/p' "$2" | head -n 1; }
if has power; then
	hub_device="$(json_value hub_device_id "$(site_file hub.json)")"
	power_device="$(json_value device_id "$(site_file power.json)")"
	[ "$hub_device" = "$power_device" ] ||
		fail "power.json device_id ($power_device) is not hub.json hub_device_id ($hub_device): the hub would never see a power cut"
fi
say "programs to run:$enabled"

# --- System packages ------------------------------------------------------------

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y mosquitto
if has video; then
	# go2rtc decodes the photos of an alarm with it.
	apt-get install -y --no-install-recommends ffmpeg
fi

# --- User and data folders ------------------------------------------------------

getent passwd safehome >/dev/null ||
	useradd --system --home-dir /var/lib/safehome --shell /usr/sbin/nologin safehome
chown safehome:safehome "$ROOT/var/lib/safehome" "$ROOT/var/lib/zigbee2mqtt"
chmod 700 "$ROOT/var/lib/safehome"
chown mosquitto:mosquitto "$ROOT/var/lib/mosquitto"

# systemd-timesyncd's clock file, the date it sets the clock forward to at boot before
# NTP, kept on the data partition (docs/systeme.md, step 2); taken from the next boot on.
# The folder and the file as it is now first, the line in fstab last: a boot never finds
# the line without its folder (deploy_test.go checks the order). It must not: through
# StateDirectory=, timesyncd requires the mounts of its folder (systemd.exec(5), implicit
# dependencies), so a failed bind leaves the hub without NTP. A hub installed before
# 28 October 2026 gets it here.
TIMESYNC=/var/lib/systemd/timesync
mkdir -p "$ROOT/data/timesync"
chmod 755 "$ROOT/data/timesync"
if [ ! -e "$ROOT/data/timesync/clock" ] && [ -f "$ROOT$TIMESYNC/clock" ]; then
	# Its date, not now: the clock may not be synced yet at the first install.
	cp -p "$ROOT$TIMESYNC/clock" "$ROOT/data/timesync/clock"
fi
# systemd gives the folder to timesyncd's user at each start anyway (StateDirectory=).
if getent passwd systemd-timesync >/dev/null; then
	chown -R systemd-timesync:systemd-timesync "$ROOT/data/timesync"
fi
# timesync_line FILE: the line of FILE that mounts on $TIMESYNC.
timesync_line() { awk -v m="$TIMESYNC" '$1 !~ /^#/ && $2 == m' "$1" 2>/dev/null; }
fstab="$ROOT/etc/fstab"
if [ -z "$(timesync_line "$fstab")" ]; then
	line="$(timesync_line "$BUNDLE/deploy/system/fstab-data")"
	[ -n "$line" ] || fail "the bundle's fstab-data binds nothing on $TIMESYNC"
	{
		if [ -s "$fstab" ]; then
			cat "$fstab"
			[ -z "$(tail -c 1 "$fstab")" ] || echo
		fi
		printf '%s\n' "$line"
	} >"$fstab.new"
	chmod 644 "$fstab.new"
	mv -f "$fstab.new" "$fstab"
	say "$TIMESYNC bound to /data/timesync in /etc/fstab, from the next boot on"
fi

# put SRC DEST MODE: replaces DEST in one step (a running program keeps its old file).
put() {
	mkdir -p "$(dirname "$2")"
	install -m "$3" "$1" "$2.new"
	mv -f "$2.new" "$2"
}

# put_program SRC DEST: puts a program, keeping the one it replaces as DEST.prev when they
# differ, for update-hub.sh to put back if the services do not hold (docs/systeme.md,
# "Maintenance"). An unchanged or new program leaves no .prev (those of an earlier update
# are gone before the mark, below). While $UNCONFIRMED exists, the programs in place are
# an install not yet confirmed (a run cut short, or phase 2 run again): its .prev are the
# programs that held, kept as they are. A .prev is whole or absent: copied to .prev.new
# first, a copy cut short is never taken for the way back.
put_program() {
	if [ -n "$unconfirmed" ] && [ -e "$2.prev" ]; then
		:
	elif [ -f "$2" ] && ! cmp -s "$1" "$2"; then
		cp -p "$2" "$2.prev.new"
		mv -f "$2.prev.new" "$2.prev"
	fi
	put "$1" "$2" 755
}

if [ -x "$BUNDLE/bin/go2rtc" ]; then
	put "$BUNDLE/bin/go2rtc" "$BIN/go2rtc" 755
fi

# --- Configuration, checked with the new programs -------------------------------

mkdir -p "$ETC"
# Always: the profiles below are replaced even without a site/ folder.
backup="$ETC.prev"
rm -rf "$backup"
cp -a "$ETC" "$backup"
# The device profiles come with the programs; a site may add its own.
rm -rf "$ETC/profiles"
cp -R "$BUNDLE/config/profiles" "$ETC/profiles"
if [ -d "$SITE" ]; then
	cp -R "$SITE/." "$ETC/"
fi
if has maint && [ ! -f "$ETC/maint.key" ]; then
	# The hub's key for the tunnel only (docs/maintenance.md); no passphrase: the
	# service uses it alone.
	ssh-keygen -q -t ed25519 -N "" -C "$(hostname)" -f "$ETC/maint.key"
fi
if has console && [ ! -f "$ETC/console.secret" ]; then
	# The passphrase of the console's Wi-Fi, for the hub's label: 12 characters
	# without look-alikes (no l, o, 0, 1).
	raw="$(head -c 512 /dev/urandom | LC_ALL=C tr -dc 'a-km-np-z2-9')"
	printf '%s\n' "${raw:0:12}" >"$ETC/console.secret"
fi
chown -R root:safehome "$ETC"
chmod -R u=rwX,g=rX,o= "$ETC" # contacts, cloud secret, camera passwords

failed=""
for p in $enabled; do
	"$BUNDLE/bin/safehome-$p" check -config "$ETC/$p.json" || failed="$failed $p.json"
done
if [ -n "$failed" ]; then
	rm -rf "$ETC"
	mv "$backup" "$ETC"
	fail "bad configuration:$failed. The previous files are back; nothing was restarted"
fi
rm -rf "$backup"

# --- System settings, broker, programs ------------------------------------------

put "$BUNDLE/deploy/system/journald-safehome.conf" "$ROOT/etc/systemd/journald.conf.d/safehome.conf" 644
put "$BUNDLE/deploy/system/watchdog-safehome.conf" "$ROOT/etc/systemd/system.conf.d/safehome-watchdog.conf" 644
put "$BUNDLE/deploy/mosquitto/mosquitto.conf" "$ROOT/etc/mosquitto/mosquitto.conf" 644
if has console; then
	# The console makes its network through NetworkManager, as the service's user,
	# and this script keeps the network from routing the phones anywhere.
	put "$BUNDLE/deploy/system/polkit-safehome-console.rules" "$ROOT/etc/polkit-1/rules.d/50-safehome-console.rules" 644
	put "$BUNDLE/deploy/system/nm-dispatcher-safehome-console" "$ROOT/etc/NetworkManager/dispatcher.d/50-safehome-console" 755
fi

# Made before the first program is replaced, removed by update-hub.sh once the new ones
# hold (or by hand, step 6 of "Maintenance"); a run that keeps no .prev leaves none. It
# holds a token of this run: update-hub.sh tells it from a mark left by an earlier run
# cut short, which does not mean this one replaced anything.
UNCONFIRMED="$BIN/.safehome-prev.unconfirmed"
unconfirmed=""
[ ! -e "$UNCONFIRMED" ] || unconfirmed=yes
mkdir -p "$BIN"
rm -f "$BIN"/safehome-*.prev.new # copies cut short
if [ -z "$unconfirmed" ]; then
	# The programs in place held: a .prev of an earlier update is older than them. Gone
	# before the mark, so that a run cut short never leaves one to be kept as the way back.
	rm -f "$BIN"/safehome-*.prev
fi
head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$UNCONFIRMED"
chmod 644 "$UNCONFIRMED" # an earlier mark keeps its mode through >
for p in $PROGRAMS $TOOLS; do
	put_program "$BUNDLE/bin/safehome-$p" "$BIN/safehome-$p"
done
ls "$BIN"/safehome-*.prev >/dev/null 2>&1 || rm -f "$UNCONFIRMED"
for u in "$BUNDLE"/deploy/systemd/*.service; do
	put "$u" "$UNITS/$(basename "$u")" 644
done
# The clock as it reads at boot, before NTP (docs/engine.md): taken from the next boot on.
put "$BUNDLE/deploy/system/safehome-boot-clock.service" "$UNITS/safehome-boot-clock.service" 644

systemctl restart systemd-journald
systemctl daemon-reexec # the hardware watchdog
systemctl daemon-reload
systemctl enable safehome-boot-clock
systemctl enable mosquitto
systemctl restart mosquitto

# --- Zigbee2MQTT ----------------------------------------------------------------

Z2M="$ROOT/var/lib/zigbee2mqtt/configuration.yaml"
z2m_todo=""
if [ ! -d "$ROOT/opt/zigbee2mqtt" ]; then
	say "Zigbee2MQTT is not installed: do step 8 of docs/systeme.md, then run again"
else
	if [ ! -f "$Z2M" ]; then
		# Only the first time: from then on the file holds the Zigbee network key.
		install -m 600 "$BUNDLE/deploy/zigbee2mqtt/configuration.yaml" "$Z2M"
		chown safehome:safehome "$Z2M"
	fi
	if grep -q CHANGE-ME "$Z2M"; then
		# Not even enabled: with CHANGE-ME as its stick it would fail every 5 s.
		z2m_todo="set the stick's port and adapter in /var/lib/zigbee2mqtt/configuration.yaml, then: sudo systemctl enable --now zigbee2mqtt"
		say "Zigbee2MQTT not started: $z2m_todo"
	else
		systemctl enable zigbee2mqtt
		systemctl restart zigbee2mqtt
	fi
fi

# --- Start ----------------------------------------------------------------------

for p in $enabled; do
	systemctl enable "safehome-$p"
	systemctl restart "safehome-$p"
done

if has maint; then
	say "the hub's tunnel key, for the bastion (docs/maintenance.md):"
	cat "$ETC/maint.key.pub"
fi
if has console; then
	say "the console's Wi-Fi passphrase, for the hub's label with the network's name (docs/security-install.md):"
	cat "$ETC/console.secret"
fi
[ -z "$z2m_todo" ] || say "first, Zigbee2MQTT: $z2m_todo"
say "done. Then: systemctl --failed (must be empty), access codes and devices (steps 9 and 10), read-only system last (step 12)"
