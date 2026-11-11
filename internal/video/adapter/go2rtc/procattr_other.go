//go:build !linux

package go2rtc

import "syscall"

// sysProcAttr: outside Linux (development machines), nothing more.
func sysProcAttr() *syscall.SysProcAttr { return nil }
