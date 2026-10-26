package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Holders exclude each other, in one process too, and a lock held too long is
// reported, not waited for forever.
func TestHoldersTakeTurns(t *testing.T) {
	dir := t.TempDir()
	var inside, most atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := Hold(dir, "codes.lock", 10*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			n := inside.Add(1)
			for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
			}
			time.Sleep(5 * time.Millisecond)
			inside.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if most.Load() != 1 {
		t.Fatalf("%d holders at once", most.Load())
	}
	release, err := Hold(dir, "codes.lock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := Hold(dir, "codes.lock", 50*time.Millisecond); err == nil {
		t.Fatal("a held lock taken again")
	}
}

// A lock file made by a tool run as another user than the folder's owner (plain sudo)
// gets the folder's owner: left to root, read-write for root alone, the console could
// never take it again. Only root can give a file away, so here, for a folder said to
// be root's, the lock is refused with the reason, and no file is left in the way.
func TestTheLockFileGetsTheFoldersOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can give the file away")
	}
	dir := t.TempDir()
	_, err := hold(dir, "codes.lock", time.Second, &syscall.Stat_t{Uid: 0, Gid: 0})
	if err == nil || !strings.Contains(err.Error(), "run the tool as that user") {
		t.Fatalf("a lock owned by the wrong user was taken: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "codes.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lock file is left: %v", err)
	}
	info, _ := os.Stat(dir)
	release, err := hold(dir, "codes.lock", time.Second, info.Sys().(*syscall.Stat_t))
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// A lock file planted as a link, or as another name of some other file, is refused and
// left as it is: run as root (sudo safehome-hub codes set), the lock must never give
// the link's target to the folder's owner.
func TestALinkIsNeverTakenForTheLock(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(victim)
	lock := filepath.Join(dir, "codes.lock")
	if err := os.Symlink(victim, lock); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(dir)
	owner := &syscall.Stat_t{Uid: 65534, Gid: 65534} // as root, a folder of another user
	if os.Geteuid() != 0 {
		owner = info.Sys().(*syscall.Stat_t)
	}
	if release, err := hold(dir, "codes.lock", 50*time.Millisecond, owner); err == nil {
		release()
		t.Fatal("a symbolic link was taken for the lock")
	}
	if fi, err := os.Lstat(lock); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link is left as it is: %v", err)
	}
	os.Remove(lock)
	if err := os.Link(victim, lock); err != nil {
		t.Fatal(err)
	}
	if release, err := hold(dir, "codes.lock", 50*time.Millisecond, owner); err == nil {
		release()
		t.Fatal("a hard link was taken for the lock")
	}
	after, _ := os.Stat(victim)
	b, a := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if a.Uid != b.Uid || a.Gid != b.Gid {
		t.Fatalf("the target was given away: %d:%d, was %d:%d", a.Uid, a.Gid, b.Uid, b.Gid)
	}
}
