package go2rtc

import "syscall"

// sysProcAttr kills go2rtc if the video service dies without stopping it: a stream
// left running would spend the client's mobile data.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
