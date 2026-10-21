#!/usr/bin/env bash
# Makes the bundle that deploy/install.sh installs on a hub (docs/systeme.md). Run on
# the computer, from anywhere in the repository:
#
#   deploy/bundle.sh -v 0.1.0 -s ~/sites/tg-lom-0001 -g ~/Downloads/go2rtc_linux_arm64 dist/tg-lom-0001
#
#   -v VERSION  written into the programs (safehome-hub version)
#   -s SITE     the site's configuration: hub.json, gsm.json, zigbee.json, agent.json,
#               cloud.secret, and video.json, maint.json, maint_known_hosts, power.json if used
#   -g GO2RTC   go2rtc for linux/arm64, checked beforehand (site with cameras only)
#
# Then: scp -r dist/tg-lom-0001 admin@safehome-tg-lom-0001:safehome
#       ssh admin@safehome-tg-lom-0001 sudo ./safehome/deploy/install.sh
# The bundle holds the site's secrets: delete it from the computer once installed.
set -euo pipefail

usage() {
	echo "usage: deploy/bundle.sh [-v VERSION] [-s SITE] [-g GO2RTC] OUT" >&2
	exit 2
}

version=dev
site=""
go2rtc=""
while getopts "v:s:g:" opt; do
	case "$opt" in
	v) version="$OPTARG" ;;
	s) site="$OPTARG" ;;
	g) go2rtc="$OPTARG" ;;
	*) usage ;;
	esac
done
shift $((OPTIND - 1))
[ $# = 1 ] || usage
out="$1"

repo="$(cd "$(dirname "$0")/.." && pwd)"
[ ! -e "$out" ] || {
	echo "bundle.sh: $out already exists" >&2
	exit 1
}
mkdir -p "$out/bin" "$out/config"
out="$(cd "$out" && pwd)"

for d in "$repo"/cmd/safehome-*; do
	name="$(basename "$d")"
	(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$out/bin/$name" "./cmd/$name")
done

cp -R "$repo/deploy" "$out/deploy"
rm -f "$out/deploy"/*.go "$out/deploy/bundle.sh"
cp -R "$repo/config/profiles" "$out/config/profiles"
cp "$repo"/config/*.example.json "$out/config/"

if [ -n "$go2rtc" ]; then
	install -m 755 "$go2rtc" "$out/bin/go2rtc"
fi
if [ -n "$site" ]; then
	mkdir -p "$out/site"
	cp -R "$site/." "$out/site/"
	chmod -R go= "$out/site"
fi
echo "bundle.sh: $out ($version)"
