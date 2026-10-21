#!/usr/bin/env bash
# Builds the bundle of the current tree with the virtual site (vkit/site) and the
# fake devices, and installs it on the virtual Pi made by vkit/up.sh
# (docs/kit-virtuel.md). It can be run again, as an update would be.
#
#   vkit/deploy.sh
set -euo pipefail

VM=safehome-pi
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# go2rtc for the hub, as a real site with cameras gets it (version and digest pinned).
GO2RTC_VERSION=v1.9.14
GO2RTC_SHA256=359fabade8a7a51e81a55fe6df6b0ef81764a5e1d63179577534eaaa71904b50
cache="${XDG_CACHE_HOME:-$HOME/.cache}/safehome-vkit"
go2rtc="$cache/go2rtc_linux_arm64-$GO2RTC_VERSION"
if [ ! -f "$go2rtc" ]; then
	mkdir -p "$cache"
	curl -fsSL -o "$go2rtc.part" "https://github.com/AlexxIT/go2rtc/releases/download/$GO2RTC_VERSION/go2rtc_linux_arm64"
	echo "$GO2RTC_SHA256  $go2rtc.part" | shasum -a 256 -c - >/dev/null
	mv "$go2rtc.part" "$go2rtc"
fi

"$repo/deploy/bundle.sh" -v vkit -s "$here/site" -g "$go2rtc" "$work/bundle"
mkdir -p "$work/vkit/bin"
for tool in fakemodem fakez2m; do
	(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o "$work/vkit/bin/vkit-$tool" "./vkit/$tool")
done
cp -R "$here/systemd" "$here/cloud.conf" "$here/camera.yaml" "$here/install-kit.sh" "$work/vkit/"

limactl shell "$VM" -- rm -rf /tmp/vkit-deploy
limactl copy -r "$work" "$VM:/tmp/vkit-deploy"
limactl shell "$VM" sudo bash /tmp/vkit-deploy/vkit/install-kit.sh /tmp/vkit-deploy
limactl shell "$VM" sudo rm -rf /tmp/vkit-deploy # the bundle holds the site's secrets
