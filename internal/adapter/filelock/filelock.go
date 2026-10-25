// Package filelock serialises the programs that change the same files: the local
// console and the command-line tool both rewrite the hub's access codes from their
// own copy (read, change, write the whole list); without a lock, the second write
// drops the first one's change, though both said it was saved.
package filelock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// retry is how often a lock held by another is tried again.
const retry = 20 * time.Millisecond

// Hold takes the lock file name in dir, waiting up to wait for another holder to let
// it go; release lets it go. The lock is the system's (flock): a holder that dies
// drops it, and two holders in one process exclude each other too (each opens the
// file). The file stays: removing it would let a later holder lock another file.
func Hold(dir, name string, wait time.Duration) (release func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filelock: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || !time.Now().Before(deadline) {
			f.Close()
			return nil, fmt.Errorf("filelock: %s is held by another program: %w", name, err)
		}
		time.Sleep(retry)
	}
}
