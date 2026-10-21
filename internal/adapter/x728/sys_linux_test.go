package x728

import (
	"testing"
	"unsafe"
)

// The structures handed to the kernel have the sizes of linux/gpio.h and
// linux/i2c-dev.h on a 64-bit Pi, and the requests their numbers.
func TestTheKernelStructuresHaveTheirSizes(t *testing.T) {
	for _, c := range []struct {
		what      string
		got, want uintptr
	}{
		{"gpiochip_info", unsafe.Sizeof(chipInfo{}), 68},
		{"gpio_v2_line_attribute", unsafe.Sizeof(lineAttribute{}), 16},
		{"gpio_v2_line_config", unsafe.Sizeof(lineConfig{}), 272},
		{"gpio_v2_line_request", unsafe.Sizeof(lineRequest{}), 592},
		{"gpio_v2_line_values", unsafe.Sizeof(lineValues{}), 16},
		{"i2c_smbus_ioctl_data", unsafe.Sizeof(smbusIoctlData{}), 16},
	} {
		if c.got != c.want {
			t.Errorf("%s: %d bytes, want %d", c.what, c.got, c.want)
		}
	}
	for _, c := range []struct {
		what      string
		got, want uint
	}{
		{"GPIO_GET_CHIPINFO_IOCTL", ioctlChipInfo, 0x8044B401},
		{"GPIO_V2_GET_LINE_IOCTL", ioctlGetLine, 0xC250B407},
		{"GPIO_V2_LINE_GET_VALUES_IOCTL", ioctlGetValues, 0xC010B40E},
		{"GPIO_V2_LINE_SET_VALUES_IOCTL", ioctlSetValues, 0xC010B40F},
	} {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.what, c.got, c.want)
		}
	}
}
