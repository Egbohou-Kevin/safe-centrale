// Package pidfile keeps a process from running twice on the same state: two hubs, or
// two GSM gateways, on the same files would each believe they own them. The file
// also tells the installer's tools which process to signal.
//
// The program that runs holds a lock on its pid file (flock), which the system drops
// when the program ends, however it ends. A pid written in the file is never proof
// alone: after a power cut the file stays on the data partition, and at the next boot
// its pid is given again to another program (mosquitto, another service of ours),
// which must neither keep the hub from starting nor receive its signals.
package pidfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Lock takes the pid file name in dir and writes this process's pid in it, and
// refuses if another process holds it. release removes the file and drops the lock.
func Lock(dir, name string) (release func(), err error) {
	path := filepath.Join(dir, name)
	f, err := hold(path)
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	if err := f.Truncate(0); err == nil {
		_, err = f.WriteAt([]byte(strconv.Itoa(self)+"\n"), 0)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		if pid, ok := read(path); ok && pid == self {
			os.Remove(path)
		}
		f.Close()
	}, nil
}

// attempts and attemptWait: a lock taken for an instant by Held (a tool checking
// whether the program runs) must not keep the program from starting.
const (
	attempts    = 3
	attemptWait = 100 * time.Millisecond
)

// hold opens the file and locks it. The file is opened again at each attempt, and
// must still be the one at the path once locked: a release removes it, and a lock on
// a removed file would lock nothing.
func hold(path string) (*os.File, error) {
	for attempt := 1; ; attempt++ {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil && samePath(f, path) {
			return f, nil
		}
		f.Close()
		switch {
		case err != nil && !errors.Is(err, syscall.EWOULDBLOCK):
			return nil, err
		case attempt == attempts:
			pid, _ := read(path)
			return nil, fmt.Errorf("process %d is running", pid)
		}
		time.Sleep(attemptWait)
	}
}

func samePath(f *os.File, path string) bool {
	opened, err := f.Stat()
	if err != nil {
		return false
	}
	now, err := os.Stat(path)
	return err == nil && os.SameFile(opened, now)
}

// Held returns the pid of the process that holds the pid file, if one does: the pid
// written in a file nobody holds is not that of a running program of ours.
func Held(dir, name string) (int, bool) {
	path := filepath.Join(dir, name)
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		return 0, false // nobody holds it; the lock goes with the close
	}
	return read(path)
}

// read returns the pid written in the file, if there is a valid one.
func read(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, err == nil && pid > 0
}
