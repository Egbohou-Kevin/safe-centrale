package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The update announcement is read from the state folder as deploy/update-hub.sh writes
// it: none without the file, its time and when it was written with it, an error for a
// file that does not hold a time (the hub then announces nothing).
func TestTheUpdateAnnouncementIsReadFromTheStateFolder(t *testing.T) {
	dir := t.TempDir()
	read := readUpdating(dir)
	if until, made, err := read(); err != nil || !until.IsZero() || !made.IsZero() {
		t.Fatalf("without the file: %v %v %v, want none", until, made, err)
	}

	path := filepath.Join(dir, updatingFile)
	if err := os.WriteFile(path, []byte("2026-10-29T20:53:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	written := time.Date(2026, 10, 29, 20, 33, 0, 0, time.UTC)
	if err := os.Chtimes(path, written, written); err != nil {
		t.Fatal(err)
	}
	until, made, err := read()
	if want := time.Date(2026, 10, 29, 20, 53, 0, 0, time.UTC); err != nil || !until.Equal(want) || !made.Equal(written) {
		t.Fatalf("got %v written %v (%v), want %v written %v", until, made, err, want, written)
	}

	for _, bad := range []string{"", "soon\n", "1200\n", "2026-10-29 20:53\n"} {
		if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if until, _, err := read(); err == nil {
			t.Errorf("%q read as %v, want an error", bad, until)
		}
	}
}
