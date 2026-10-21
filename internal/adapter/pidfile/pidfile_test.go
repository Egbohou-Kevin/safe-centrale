package pidfile

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLock(t *testing.T) {
	dir := t.TempDir()
	release, err := Lock(dir, "x.pid")
	if err != nil {
		t.Fatal(err)
	}
	if pid, ok := Read(dir, "x.pid"); !ok || pid != os.Getpid() {
		t.Fatalf("%d %v", pid, ok)
	}
	// The same process may take it again (a restart in the same process, in tests).
	if _, err := Lock(dir, "x.pid"); err != nil {
		t.Fatal(err)
	}
	release()
	if _, ok := Read(dir, "x.pid"); ok {
		t.Fatal("released")
	}

	// Another live process holds it: refused.
	os.WriteFile(filepath.Join(dir, "x.pid"), []byte(strconv.Itoa(os.Getppid())+"\n"), 0o600)
	if _, err := Lock(dir, "x.pid"); err == nil {
		t.Fatal("a live process holds the file")
	}
	// A dead one does not count.
	os.WriteFile(filepath.Join(dir, "x.pid"), []byte("999999\n"), 0o600)
	if _, err := Lock(dir, "x.pid"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "x.pid"), []byte("junk"), 0o600)
	if _, ok := Read(dir, "x.pid"); ok {
		t.Fatal("junk is no pid")
	}
	if _, err := Lock(filepath.Join(dir, "missing"), "x.pid"); err == nil {
		t.Fatal("a missing directory")
	}
	if Alive(999999) {
		t.Fatal("no such process")
	}
}

// A process of another user (here init, pid 1) exists, though it cannot be
// signalled: its pid file must still count.
func TestAProcessOfAnotherUserIsAlive(t *testing.T) {
	if !Alive(1) {
		t.Fatal("pid 1 always runs")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.pid"), []byte("1\n"), 0o600)
	if _, err := Lock(dir, "x.pid"); err == nil {
		t.Fatal("a live process of another user holds the file")
	}
}

// A release must not remove a file another process wrote since.
func TestReleaseLeavesAnotherProcesssFile(t *testing.T) {
	dir := t.TempDir()
	release, err := Lock(dir, "x.pid")
	if err != nil {
		t.Fatal(err)
	}
	other := strconv.Itoa(os.Getppid()) + "\n"
	os.WriteFile(filepath.Join(dir, "x.pid"), []byte(other), 0o600)
	release()
	if pid, ok := Read(dir, "x.pid"); !ok || pid != os.Getppid() {
		t.Fatal("the release removed another process's pid file")
	}
}
