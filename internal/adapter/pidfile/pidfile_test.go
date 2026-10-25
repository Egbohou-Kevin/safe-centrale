package pidfile

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.pid")
	release, err := Lock(dir, "x.pid")
	if err != nil {
		t.Fatal(err)
	}
	if pid, ok := read(path); !ok || pid != os.Getpid() {
		t.Fatalf("%d %v", pid, ok)
	}
	if pid, ok := Held(dir, "x.pid"); !ok || pid != os.Getpid() {
		t.Fatalf("held: %d %v", pid, ok)
	}
	// Held: a second copy is refused, and leaves the file alone.
	if _, err := Lock(dir, "x.pid"); err == nil {
		t.Fatal("the file is held")
	}
	if pid, _ := read(path); pid != os.Getpid() {
		t.Fatal("a refused copy wrote the file")
	}
	release()
	if _, ok := read(path); ok {
		t.Fatal("released")
	}
	if _, ok := Held(dir, "x.pid"); ok {
		t.Fatal("no file: nobody holds it")
	}
	again, err := Lock(dir, "x.pid")
	if err != nil {
		t.Fatalf("taken again once released: %v", err)
	}
	again()

	os.WriteFile(path, []byte("junk"), 0o600)
	if _, ok := read(path); ok {
		t.Fatal("junk is no pid")
	}
	if _, err := Lock(filepath.Join(dir, "missing"), "x.pid"); err == nil {
		t.Fatal("a missing directory")
	}
}

// After a power cut, the pid file stays, and its pid is given again at the next boot,
// to a program that runs (here init, and the test's parent): nobody holds the file,
// so it does not keep the program from starting, and that program is not the one to
// signal.
func TestAPidFileLeftByACutDoesNotBlock(t *testing.T) {
	for _, pid := range []int{1, os.Getppid()} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "x.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o600)
		if _, ok := Held(dir, "x.pid"); ok {
			t.Fatalf("pid %d: a file nobody holds", pid)
		}
		release, err := Lock(dir, "x.pid")
		if err != nil {
			t.Fatalf("pid %d: %v", pid, err)
		}
		release()
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
	if pid, ok := read(filepath.Join(dir, "x.pid")); !ok || pid != os.Getppid() {
		t.Fatal("the release removed another process's pid file")
	}
}
