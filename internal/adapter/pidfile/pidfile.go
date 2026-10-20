// Package pidfile keeps a process from running twice on the same state: two hubs, or
// two GSM gateways, on the same files would each believe they own them. The file
// also tells the installer's tools which process to signal.
package pidfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Lock writes the pid file name in dir, and refuses if a live process other than
// this one already holds it. release removes the file.
func Lock(dir, name string) (release func(), err error) {
	path := filepath.Join(dir, name)
	if pid, ok := Read(dir, name); ok && pid != os.Getpid() && Alive(pid) {
		return nil, fmt.Errorf("process %d is running", pid)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return nil, err
	}
	return func() { os.Remove(path) }, nil
}

// Read returns the pid written in the file, if there is a valid one.
func Read(dir, name string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, err == nil && pid > 0
}

// Alive reports whether a process exists (signal 0 only checks).
func Alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
