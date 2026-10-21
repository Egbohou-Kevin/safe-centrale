#!/usr/bin/env bash
# Runs inside the virtual Pi (vkit/up.sh copies and starts it): what an installer
# does by hand on a real Pi before deploy/install.sh (docs/systeme.md, step 2), plus
# what the virtual kit needs instead of the hardware. It can be run again.
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
apt-get update -q
# linux-image-arm64: the cloud kernel has neither the simulated Wi-Fi radios (the
# console's hotspot) nor softdog (the watchdog). The rest is the kit's tooling.
apt-get install -y -q linux-image-arm64 network-manager iw mosquitto-clients openssl

# Boot on the standard kernel, not the cloud one.
sub="$(grep -o "submenu .*\$menuentry_id_option '[^']*" /boot/grub/grub.cfg | grep -o "gnulinux-advanced-[^']*")"
kernel="$(ls /boot/vmlinuz-*-arm64 | grep -v cloud | sort -V | tail -1)"
kernel="${kernel#/boot/vmlinuz-}"
echo "GRUB_DEFAULT=\"$sub>gnulinux-$kernel-advanced-${sub#gnulinux-advanced-}\"" >/etc/default/grub.d/99-safehome-vkit.cfg
update-grub

# Two simulated Wi-Fi radios (wlan0 for the console's hotspot, wlan1 for a phone)
# and a software watchdog in place of the Pi's.
printf 'mac80211_hwsim\nsoftdog\n' >/etc/modules-load.d/safehome-vkit.conf
echo 'options mac80211_hwsim radios=2' >/etc/modprobe.d/safehome-vkit.conf

# Step 2: the data partition, on the VM's second disk, mounted as on the Pi.
if ! blkid -L safehome-data >/dev/null; then
	mkfs.ext4 -q -L safehome-data /dev/vdb
fi
mkdir -p /data /var/lib/safehome /var/lib/mosquitto /var/lib/zigbee2mqtt
if ! grep -q safehome-data /etc/fstab; then
	grep -v '^#' /tmp/fstab-data >>/etc/fstab
fi
systemctl daemon-reload
mountpoint -q /data || mount /data
mkdir -p /data/safehome /data/mosquitto /data/zigbee2mqtt
mount -a
echo "prepare-pi.sh: done; restart the VM to boot on the standard kernel"
