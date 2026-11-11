package x728

// What the module needs from Linux: the GPIO character device and the I2C bus.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// The GPIO character device's interface, version 2 (linux/gpio.h).

type chipInfo struct {
	Name  [32]byte
	Label [32]byte
	Lines uint32
}

type lineAttribute struct {
	ID      uint32
	Padding uint32
	Value   uint64 // flags, output values or debounce period, after ID
}

type lineConfigAttribute struct {
	Attr lineAttribute
	Mask uint64
}

type lineConfig struct {
	Flags    uint64
	NumAttrs uint32
	Padding  [5]uint32
	Attrs    [10]lineConfigAttribute
}

type lineRequest struct {
	Offsets         [64]uint32
	Consumer        [32]byte
	Config          lineConfig
	NumLines        uint32
	EventBufferSize uint32
	Padding         [5]uint32
	FD              int32
}

type lineValues struct {
	Bits uint64
	Mask uint64
}

const (
	lineFlagInput  = 1 << 2
	lineFlagOutput = 1 << 3

	// _IOR(0xB4, 0x01, struct gpiochip_info) and the _IOWR of the line requests.
	ioctlChipInfo  = 2<<30 | uint(unsafe.Sizeof(chipInfo{}))<<16 | 0xB4<<8 | 0x01
	ioctlGetLine   = 3<<30 | uint(unsafe.Sizeof(lineRequest{}))<<16 | 0xB4<<8 | 0x07
	ioctlGetValues = 3<<30 | uint(unsafe.Sizeof(lineValues{}))<<16 | 0xB4<<8 | 0x0E
	ioctlSetValues = 3<<30 | uint(unsafe.Sizeof(lineValues{}))<<16 | 0xB4<<8 | 0x0F
)

func ioctl(fd uintptr, req uint, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(req), uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// gpioLine is a line requested from the kernel: its own file descriptor.
type gpioLine struct{ f *os.File }

// openLine requests one line of the chip labelled label, as an input or as an
// output held low.
func openLine(label string, offset int, output bool) (line, error) {
	chip, err := findChip(label)
	if err != nil {
		return nil, err
	}
	defer chip.Close()
	req := lineRequest{NumLines: 1}
	req.Offsets[0] = uint32(offset)
	copy(req.Consumer[:], "safehome-power")
	req.Config.Flags = lineFlagInput
	if output {
		req.Config.Flags = lineFlagOutput
	}
	if err := ioctl(chip.Fd(), ioctlGetLine, unsafe.Pointer(&req)); err != nil {
		return nil, fmt.Errorf("requesting line %d of %s: %w", offset, label, err)
	}
	return gpioLine{os.NewFile(uintptr(req.FD), fmt.Sprintf("%s line %d", label, offset))}, nil
}

// findChip opens the GPIO chip whose label is label.
func findChip(label string) (*os.File, error) {
	paths, _ := filepath.Glob("/dev/gpiochip*")
	var seen []string
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			seen = append(seen, fmt.Sprintf("%s (%v)", p, err))
			continue
		}
		var info chipInfo
		if err := ioctl(f.Fd(), ioctlChipInfo, unsafe.Pointer(&info)); err == nil && cString(info.Label[:]) == label {
			return f, nil
		}
		seen = append(seen, fmt.Sprintf("%s %q", p, cString(info.Label[:])))
		f.Close()
	}
	return nil, fmt.Errorf("no GPIO chip labelled %q (seen: %v)", label, seen)
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func (l gpioLine) get() (bool, error) {
	v := lineValues{Mask: 1}
	if err := ioctl(l.f.Fd(), ioctlGetValues, unsafe.Pointer(&v)); err != nil {
		return false, err
	}
	return v.Bits&1 == 1, nil
}

func (l gpioLine) set(high bool) error {
	v := lineValues{Mask: 1}
	if high {
		v.Bits = 1
	}
	return ioctl(l.f.Fd(), ioctlSetValues, unsafe.Pointer(&v))
}

func (l gpioLine) close() error { return l.f.Close() }

// The I2C bus's interface (linux/i2c-dev.h): the gauge is read with SMBus word
// reads, which every adapter offers.

type smbusData [34]byte // union i2c_smbus_data

type smbusIoctlData struct {
	ReadWrite uint8
	Command   uint8
	Size      uint32
	Data      *smbusData
}

const (
	ioctlI2CSlave = 0x0703
	ioctlSMBus    = 0x0720
	smbusRead     = 1
	smbusWordData = 3
)

type i2cDevice struct{ f *os.File }

func openSMBus(path string, addr int) (smbus, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioctlI2CSlave, uintptr(addr)); errno != 0 {
		f.Close()
		return nil, fmt.Errorf("address %#x: %w", addr, errno)
	}
	return i2cDevice{f}, nil
}

func (d i2cDevice) readWord(reg byte) (uint16, error) {
	var data smbusData
	args := smbusIoctlData{ReadWrite: smbusRead, Command: reg, Size: smbusWordData, Data: &data}
	if err := ioctl(d.f.Fd(), ioctlSMBus, unsafe.Pointer(&args)); err != nil {
		if errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.EREMOTEIO) {
			return 0, fmt.Errorf("register %#x: nothing answers at the gauge's address: %w", reg, err)
		}
		return 0, fmt.Errorf("register %#x: %w", reg, err)
	}
	return uint16(data[0]) | uint16(data[1])<<8, nil
}

func (d i2cDevice) close() error { return d.f.Close() }
