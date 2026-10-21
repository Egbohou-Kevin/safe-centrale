#!/usr/bin/env bash
# Creates and prepares the virtual Pi (docs/kit-virtuel.md), on a Mac with Apple
# silicon and Lima (brew install lima). It can be run again: an existing VM is only
# started.
#
#   vkit/up.sh
set -euo pipefail

VM=safehome-pi
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/.." && pwd)"

if limactl list -q | grep -qx "$VM"; then
	limactl start "$VM" >/dev/null
	echo "up.sh: $VM is running"
	exit 0
fi

# The data partition of the SD card, as a second disk.
limactl disk list -q 2>/dev/null | grep -qx safehome-data ||
	limactl disk create safehome-data --size 4GiB --format raw
limactl start --name="$VM" --tty=false "$here/lima.yaml"

limactl copy "$repo/deploy/system/fstab-data" "$VM:/tmp/fstab-data"
limactl copy "$here/prepare-pi.sh" "$VM:/tmp/prepare-pi.sh"
limactl shell "$VM" sudo bash /tmp/prepare-pi.sh
limactl stop "$VM"
limactl start "$VM"
limactl shell "$VM" -- bash -c 'uname -r; ls /dev/watchdog; iw dev | grep -c Interface'
echo "up.sh: $VM is ready; next: vkit/deploy.sh"
