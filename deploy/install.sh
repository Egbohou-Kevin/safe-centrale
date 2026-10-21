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

# The services under cmd/, and the tools run by hand (deploy_test.go checks both lists).
PROGRAMS="hub gsm zigbee agent video maint console power"
TOOLS="install"
# Every site runs these; video (cameras), maint (remote maintenance), console (the
# local console) and power (the battery module) only when their configuration is there.
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

enabled=""
missing=""
for p in $PROGRAMS; do
	if configured "$p.json"; then
		enabled="$enabled $p"
	else
		case " $REQUIRED " in *" $p "*) missing="$missing $p.json" ;; esac
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
site_file() { if [ -f "$SITE/$1" ]; then echo "$SITE/$1"; else echo "$ETC/$1"; fi; }
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

# put SRC DEST MODE: replaces DEST in one step (a running program keeps its old file).
put() {
	mkdir -p "$(dirname "$2")"
	install -m "$3" "$1" "$2.new"
	mv -f "$2.new" "$2"
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
	# The console makes its network through NetworkManager, as the service's user.
	put "$BUNDLE/deploy/system/polkit-safehome-console.rules" "$ROOT/etc/polkit-1/rules.d/50-safehome-console.rules" 644
fi

for p in $PROGRAMS $TOOLS; do
	put "$BUNDLE/bin/safehome-$p" "$BIN/safehome-$p" 755
done
for u in "$BUNDLE"/deploy/systemd/*.service; do
	put "$u" "$UNITS/$(basename "$u")" 644
done

systemctl restart systemd-journald
systemctl daemon-reexec # the hardware watchdog
systemctl daemon-reload
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
