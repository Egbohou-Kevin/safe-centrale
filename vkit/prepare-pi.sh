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

# The battery module's lines and fuel gauge (vkit-power): the kernel's simulators,
# gpio-sim and i2c-stub, which Debian does not build. Their sources, pinned, are built
# for the standard kernel. irq_sim (which gpio-sim needs, not built either) goes into
# gpio-sim's module, with the one call to a function the kernel does not export
# replaced by its exported equivalent.
KERNEL_TAG=v6.12.113
KERNEL_SOURCES=(
	"drivers/gpio/gpio-sim.c e97b27cb38d9f13245eafd983bbc8d2b97837e052d4a7c65c7960eefd026b909"
	"drivers/i2c/i2c-stub.c da8d98eeb24b6c306b630a94a02655e84713f73df375894ca7a4a17e86ec1a69"
	"kernel/irq/irq_sim.c 808a37813c4f58a567b22f30c408b912042e137572e7e143f1f59274387de879"
)
if [ "v${kernel%%+*}" != "$KERNEL_TAG" ]; then
	echo "prepare-pi.sh: the simulators' sources are pinned for $KERNEL_TAG, the kernel is $kernel: update KERNEL_TAG and the digests" >&2
	exit 1
fi
apt-get install -y -q "linux-headers-$kernel" build-essential i2c-tools
sim="$(mktemp -d)"
for entry in "${KERNEL_SOURCES[@]}"; do
	read -r path digest <<<"$entry"
	curl -fsSL -o "$sim/${path##*/}" "https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git/plain/$path?h=$KERNEL_TAG"
	echo "$digest  $sim/${path##*/}" | sha256sum -c - >/dev/null
done
mv "$sim/gpio-sim.c" "$sim/gpio-sim-main.c"
sed -i 's/handle_simple_irq(irq_to_desc(irqnum));/generic_handle_irq(irqnum);/' "$sim/irq_sim.c"
printf 'obj-m += gpio-sim.o i2c-stub.o\ngpio-sim-y := gpio-sim-main.o irq_sim.o\n' >"$sim/Kbuild"
make -s -C "/lib/modules/$kernel/build" M="$sim" modules
install -D -m 644 -t "/lib/modules/$kernel/updates/vkit" "$sim/gpio-sim.ko" "$sim/i2c-stub.ko"
depmod "$kernel"
rm -rf "$sim"

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
# Every source of a bind in fstab-data, before mount -a (deploy_test.go checks it);
# timesyncd's clock file as on the Pi (docs/systeme.md, step 2).
mkdir -p /data/safehome /data/mosquitto /data/zigbee2mqtt /data/timesync /var/lib/systemd/timesync
if [ ! -e /data/timesync/clock ] && [ -f /var/lib/systemd/timesync/clock ]; then
	cp -p /var/lib/systemd/timesync/clock /data/timesync/
fi
if getent passwd systemd-timesync >/dev/null; then
	chown -R systemd-timesync:systemd-timesync /data/timesync
fi
mount -a
echo "prepare-pi.sh: done; restart the VM to boot on the standard kernel"
