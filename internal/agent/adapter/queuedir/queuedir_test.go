package queuedir

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
)

func open(t *testing.T) (*Dir, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "queue")
	d, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d, dir
}

func load(t *testing.T, d *Dir) map[string]string {
	t.Helper()
	items, err := d.Load()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for name, data := range items {
		out[name] = string(data)
	}
	return out
}

func TestPutReplaceRemoveLoad(t *testing.T) {
	d, dir := open(t)
	if got := load(t, d); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	for _, step := range []struct{ name, data string }{{"a", `{"n":1}`}, {"b", `{"n":2}`}, {"a", `{"n":3}`}} {
		if err := d.Put(step.name, []byte(step.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Remove("b", "never-there"); err != nil {
		t.Fatal(err)
	}
	if got := load(t, d); !maps.Equal(got, map[string]string{"a": `{"n":3}`}) {
		t.Fatalf("%v", got)
	}
	// One file per item, and nothing else.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "a.json" {
		t.Fatalf("%v", entries)
	}
	// Another Dir on the same folder, as after a restart, sees the same.
	again, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := load(t, again); !maps.Equal(got, map[string]string{"a": `{"n":3}`}) {
		t.Fatalf("%v", got)
	}
}

func TestBadNamesAndDataAreRefused(t *testing.T) {
	d, _ := open(t)
	for _, name := range []string{"", ".hidden", "../escape", `a\b`} {
		if err := d.Put(name, []byte(`{}`)); err == nil {
			t.Errorf("Put(%q) accepted", name)
		}
		if err := d.Remove(name); err == nil {
			t.Errorf("Remove(%q) accepted", name)
		}
	}
	if err := d.Put("a", []byte(`{"n":`)); err == nil {
		t.Error("invalid JSON accepted")
	}
	if got := load(t, d); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestWhatACrashLeftIsCleanedAtOpen(t *testing.T) {
	d, dir := open(t)
	d.Put("whole", []byte(`{"n":1}`))
	// A write cut before its rename, and an item the file system lost in a crash.
	os.WriteFile(filepath.Join(dir, "next.123456.tmp"), []byte(`{"n":`), 0o600)
	os.WriteFile(filepath.Join(dir, "torn.json"), []byte(`{"n":2`), 0o600)
	os.WriteFile(filepath.Join(dir, "empty.json"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not an item"), 0o600)

	again, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "next.123456.tmp")); !os.IsNotExist(err) {
		t.Error("the temporary file is still there")
	}
	if got := load(t, again); !maps.Equal(got, map[string]string{"whole": `{"n":1}`}) {
		t.Fatalf("%v", got)
	}
	for _, f := range []string{"torn.json", "empty.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("the damaged %s is still there", f)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Error("a file that is not an item was touched")
	}
}

func TestAFailedWriteLeavesNothingBehind(t *testing.T) {
	d, dir := open(t)
	d.Put("a", []byte(`{"n":1}`))
	os.Mkdir(filepath.Join(dir, "b.json"), 0o700) // the rename cannot replace a directory
	if err := d.Put("b", []byte(`{"n":2}`)); err == nil {
		t.Fatal("the write should fail")
	}
	tmps, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(tmps) != 0 {
		t.Fatalf("left %v", tmps)
	}
	if got := load(t, d); !maps.Equal(got, map[string]string{"a": `{"n":1}`}) {
		t.Fatalf("%v", got)
	}
}
