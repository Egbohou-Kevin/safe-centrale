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

"$repo/deploy/bundle.sh" -v vkit -s "$here/site" "$work/bundle"
mkdir -p "$work/vkit/bin"
for tool in fakemodem fakez2m; do
	(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o "$work/vkit/bin/vkit-$tool" "./vkit/$tool")
done
cp -R "$here/systemd" "$here/cloud.conf" "$here/install-kit.sh" "$work/vkit/"

limactl shell "$VM" -- rm -rf /tmp/vkit-deploy
limactl copy -r "$work" "$VM:/tmp/vkit-deploy"
limactl shell "$VM" sudo bash /tmp/vkit-deploy/vkit/install-kit.sh /tmp/vkit-deploy
limactl shell "$VM" sudo rm -rf /tmp/vkit-deploy # the bundle holds the site's secrets
