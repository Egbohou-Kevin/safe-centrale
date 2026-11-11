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
//
// The file gets the folder's owner and group, as the state files do: made by a tool
// run with plain sudo, root's alone, the service that owns the folder could never
// open it again. Only root can give a file away; anyone else is refused, and the file
// is removed rather than left in the owner's way.
func Hold(dir, name string, wait time.Duration) (release func(), err error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("filelock: %w", err)
	}
	owner, _ := info.Sys().(*syscall.Stat_t)
	return hold(dir, name, wait, owner)
}

// hold is Hold for a folder owned by owner, nil where the system has none.
func hold(dir, name string, wait time.Duration, owner *syscall.Stat_t) (release func(), err error) {
	path := filepath.Join(dir, name)
	// Never through a link: run as root, the lock would hand the link's target to the
	// folder's owner. The file opened must be a plain file of its own, too (no other
	// name for it).
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filelock: %s is not a plain file: %w", name, err)
	}
	if err := plain(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("filelock: %s: %w", name, err)
	}
	if err := ownedLike(f, owner); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("filelock: %s belongs to another user: run the tool as that user (sudo -u safehome): %w", dir, err)
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

// errNotPlain refuses a lock file that is not a plain file with one name.
var errNotPlain = errors.New("not a plain file with a single name: remove it")

// plain checks that the file opened is a plain file with no other name: a hard link
// to another file must not be given away either.
func plain(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errNotPlain
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink != 1 {
		return errNotPlain
	}
	return nil
}

// ownedLike gives the file the owner and group of the folder, unless it has them.
func ownedLike(f *os.File, owner *syscall.Stat_t) error {
	if owner == nil {
		return nil
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if cur, ok := info.Sys().(*syscall.Stat_t); ok && cur.Uid == owner.Uid && cur.Gid == owner.Gid {
		return nil
	}
	return f.Chown(int(owner.Uid), int(owner.Gid))
}
