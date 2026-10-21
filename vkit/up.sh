#!/usr/bin/env bash
# Creates and prepares the virtual Pi (docs/kit-virtuel.md), on a Mac with Apple
# silicon and Lima (brew install lima). It can be run again: an existing VM is
# started, and prepared again only if prepare-pi.sh changed since.
#
#   vkit/up.sh
set -euo pipefail

VM=safehome-pi
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/.." && pwd)"

# The VM remembers which prepare-pi.sh prepared it.
prepared=/etc/safehome-vkit-prepared
digest="$(shasum -a 256 "$here/prepare-pi.sh" | cut -d' ' -f1)"

if limactl list -q | grep -qx "$VM"; then
	limactl start "$VM" >/dev/null
	if [ "$(limactl shell "$VM" cat "$prepared" 2>/dev/null)" = "$digest" ]; then
		echo "up.sh: $VM is running"
		exit 0
	fi
	echo "up.sh: prepare-pi.sh changed: preparing $VM again"
else
	# The data partition of the SD card, as a second disk.
	limactl disk list -q 2>/dev/null | grep -qx safehome-data ||
		limactl disk create safehome-data --size 4GiB --format raw
	limactl start --name="$VM" --tty=false "$here/lima.yaml"
fi

limactl copy "$repo/deploy/system/fstab-data" "$VM:/tmp/fstab-data"
limactl copy "$here/prepare-pi.sh" "$VM:/tmp/prepare-pi.sh"
limactl shell "$VM" sudo bash /tmp/prepare-pi.sh
limactl shell "$VM" -- bash -c "echo $digest | sudo tee $prepared >/dev/null"
limactl stop "$VM"
limactl start "$VM"
limactl shell "$VM" -- bash -c 'uname -r; ls /dev/watchdog; iw dev | grep -c Interface'
echo "up.sh: $VM is ready; next: vkit/deploy.sh"
