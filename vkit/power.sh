#!/usr/bin/env bash
# The battery module of the virtual kit (docs/kit-virtuel.md): an X728 made of the
# kernel's simulators, gpio-sim for its lines and i2c-stub for its fuel gauge, which
# safehome-power reads through the same devices as on the Pi. Installed as
# /usr/local/bin/vkit-power.
#
#   vkit-power setup              at boot: the chip, the gauge, a full battery on the mains
#   vkit-power mains lost|present
#   vkit-power battery MV PERCENT the cell's voltage and the charge the gauge gives
#   vkit-power button             1 while the hub holds the module's power button
set -euo pipefail

CHIP=/sys/kernel/config/gpio-sim/x728
GAUGE=0x36

usage() {
	sed -n '7,10p' "$0" >&2
	exit 2
}

# line N: the simulator's side of line N (pull sets an input, value reads an output).
line() { echo "/sys/devices/platform/$(cat $CHIP/dev_name)/$(cat $CHIP/bank0/chip_name)/sim_gpio$1"; }

# bus: the number of the simulated I2C bus.
bus() {
	local d
	for d in /sys/class/i2c-dev/i2c-*; do
		if [ "$(cat "$d/name")" = "SMBus stub driver" ]; then
			echo "${d##*/i2c-}"
			return
		fi
	done
	echo "vkit-power: no simulated I2C bus (vkit-power setup)" >&2
	exit 1
}

# word REGISTER_VALUE: as an SMBus word write takes it, low byte first.
word() { printf '0x%04x' $(((($1 & 0xff) << 8) | ($1 >> 8))); }

case "${1:-}" in
setup)
	modprobe gpio-sim
	modprobe i2c-stub chip_addr=$GAUGE
	modprobe i2c-dev
	mountpoint -q /sys/kernel/config || mount -t configfs none /sys/kernel/config
	if [ ! -d $CHIP ]; then
		mkdir -p $CHIP/bank0
		echo 28 >$CHIP/bank0/num_lines
		echo pinctrl-bcm2711 >$CHIP/bank0/label # the Pi 4's chip, as safehome-power looks for it
		echo 1 >$CHIP/live
	fi
	udevadm settle
	ln -sfn "/dev/i2c-$(bus)" /dev/i2c-vkit
	"$0" mains present
	"$0" battery 4100 90
	;;
mains)
	case "${2:-}" in
	lost) echo pull-up >"$(line 6)/pull" ;;
	present) echo pull-down >"$(line 6)/pull" ;;
	*) usage ;;
	esac
	;;
battery)
	[ $# = 3 ] || usage
	i2cset -y "$(bus)" $GAUGE 0x02 "$(word $(($2 * 64 / 5)))" w
	i2cset -y "$(bus)" $GAUGE 0x04 "$(word $(($3 << 8)))" w
	;;
button) cat "$(line 26)/value" ;;
*) usage ;;
esac
