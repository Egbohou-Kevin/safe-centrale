package main

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
	"unsafe"
)

// openPTY opens a new pseudo-terminal and returns its master side and the path of
// its other side, the one the gateway opens as a serial port.
func openPTY() (*os.File, string, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	var unlock int32
	if err := ioctl(m, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		m.Close()
		return nil, "", err
	}
	var n uint32
	if err := ioctl(m, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		m.Close()
		return nil, "", err
	}
	slave := "/dev/pts/" + strconv.FormatUint(uint64(n), 10)
	if err := asSerialPort(slave); err != nil {
		m.Close()
		return nil, "", err
	}
	return m, slave, nil
}

// asSerialPort gives the port the owner and mode of a USB modem's: root, group
// dialout, 0660, the group the gateway's service joins.
func asSerialPort(path string) error {
	g, err := user.LookupGroup("dialout")
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return err
	}
	if err := os.Chown(path, 0, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o660)
}

func ioctl(f *os.File, req, arg uintptr) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}
