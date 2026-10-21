//go:build !linux

package x728

import "errors"

// The module is reached only through Linux's devices.
var errNotLinux = errors.New("the battery module needs Linux's GPIO and I2C devices")

func openLine(string, int, bool) (line, error) { return nil, errNotLinux }

func openSMBus(string, int) (smbus, error) { return nil, errNotLinux }
