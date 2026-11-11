package sshtunnel

import "syscall"

// sysProcAttr kills ssh if the maintenance service dies without stopping it: a
// tunnel must never outlive its session.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
