// Package pidfile keeps a process from running twice on the same state: two hubs, or
// two GSM gateways, on the same files would each believe they own them. The file
// also tells the installer's tools which process to signal.
package pidfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Lock writes the pid file name in dir, and refuses if a live process other than
// this one already holds it. release removes the file, unless another process has
// taken it since (it found this one dead): the file is then that process's.
func Lock(dir, name string) (release func(), err error) {
	self := os.Getpid()
	if pid, ok := Read(dir, name); ok && pid != self && Alive(pid) {
		return nil, fmt.Errorf("process %d is running", pid)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strconv.Itoa(self)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return func() {
		if pid, ok := Read(dir, name); ok && pid == self {
			os.Remove(path)
		}
	}, nil
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

// Alive reports whether a process exists (signal 0 only checks). A process of
// another user exists too, though this one may not signal it (EPERM).
func Alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
