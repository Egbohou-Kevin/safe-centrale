package photodir

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveOpenRemove(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "photos")
	d, err := Open(dir, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("%v", fi.Mode())
	}
	jpeg := []byte{0xFF, 0xD8, 1, 2, 3, 0xFF, 0xD9}
	if err := d.Save("01JB8ZQ7V2T4X9H6N5R0WYCE00", jpeg); err != nil {
		t.Fatal(err)
	}
	r, n, err := d.Open("01JB8ZQ7V2T4X9H6N5R0WYCE00")
	if err != nil || n != int64(len(jpeg)) {
		t.Fatalf("%d %v", n, err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, jpeg) {
		t.Fatal("not the same picture")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "01JB8ZQ7V2T4X9H6N5R0WYCE00.jpg")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v", fi.Mode())
	}
	if err := d.Remove("01JB8ZQ7V2T4X9H6N5R0WYCE00"); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove("01JB8ZQ7V2T4X9H6N5R0WYCE00"); err != nil {
		t.Fatal("gone already is fine")
	}
	if _, _, err := d.Open("01JB8ZQ7V2T4X9H6N5R0WYCE00"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	for _, bad := range []string{"", "../etc/passwd", "a/b", "x.jpg", "ab cd"} {
		if d.Save(bad, jpeg) == nil {
			t.Errorf("%q saved", bad)
		}
		if _, _, err := d.Open(bad); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%q opened: %v", bad, err)
		}
		if d.Remove(bad) == nil {
			t.Errorf("%q removed", bad)
		}
	}
	if _, err := Open(dir, 0, 1); err == nil {
		t.Fatal("limits")
	}
}

func TestTheOldestPhotosMakeRoom(t *testing.T) {
	dir := t.TempDir()
	d, _ := Open(dir, 3, 250)
	for i := range 5 {
		if err := d.Save(fmt.Sprintf("p%d", i), make([]byte, 50)); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(filepath.Join(dir, fmt.Sprintf("p%d.jpg", i)), time.Now(), time.Now().Add(time.Duration(i)*time.Second))
	}
	for i, want := range []bool{false, false, true, true, true} {
		if _, _, err := d.Open(fmt.Sprintf("p%d", i)); (err == nil) != want {
			t.Errorf("p%d kept %v", i, err == nil)
		}
	}
	// A big one pushes out the others, never itself.
	if err := d.Save("big", make([]byte, 400)); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "big.jpg" {
		t.Fatalf("%v", entries)
	}
}
