//go:build !linux

package sshtunnel

import "syscall"

// sysProcAttr: outside Linux (development machines), nothing more.
func sysProcAttr() *syscall.SysProcAttr { return nil }
