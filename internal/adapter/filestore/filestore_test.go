package filestore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestSaveThenLoad(t *testing.T) {
	s, _ := open(t)
	if err := s.Save("engine", []byte(`{"mode":"armed"}`)); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.Load("engine")
	if err != nil || !found || string(got) != `{"mode":"armed"}` {
		t.Fatalf("%q %v %v", got, found, err)
	}
}

func TestLoadOfAMissingDocumentIsNotAnError(t *testing.T) {
	s, _ := open(t)
	got, found, err := s.Load("engine")
	if err != nil || found || got != nil {
		t.Fatalf("%q %v %v", got, found, err)
	}
}

func TestSaveReplacesAndKeepsThePreviousVersion(t *testing.T) {
	s, dir := open(t)
	s.Save("engine", []byte(`{"v":1}`))
	s.Save("engine", []byte(`{"v":2}`))
	s.Save("engine", []byte(`{"v":3}`))

	got, _, _ := s.Load("engine")
	if string(got) != `{"v":3}` {
		t.Fatalf("%s", got)
	}
	prev, err := os.ReadFile(filepath.Join(dir, "engine.json.prev"))
	if err != nil || string(prev) != `{"v":2}` {
		t.Fatalf("previous version %q %v", prev, err)
	}
}

func TestNoTemporaryFileIsLeftBehind(t *testing.T) {
	s, dir := open(t)
	for i := 0; i < 5; i++ {
		s.Save("engine", []byte(`{}`))
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestOpenRemovesTemporaryFilesFromACrash(t *testing.T) {
	_, dir := open(t)
	stale := filepath.Join(dir, "engine.12345.tmp")
	os.WriteFile(stale, []byte(`{"half`), 0o600)
	if _, err := Open(dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("the stale temporary file must be removed")
	}
}

func TestDamagedCurrentFileFallsBackToThePreviousVersion(t *testing.T) {
	s, dir := open(t)
	s.Save("engine", []byte(`{"v":1}`))
	s.Save("engine", []byte(`{"v":2}`))
	os.WriteFile(filepath.Join(dir, "engine.json"), []byte(`{"v":`), 0o600) // truncated on the disk

	got, found, err := s.Load("engine")
	if err != nil || !found || string(got) != `{"v":1}` {
		t.Fatalf("%q %v %v", got, found, err)
	}
}

func TestMissingCurrentFileFallsBackToThePreviousVersion(t *testing.T) {
	s, dir := open(t)
	s.Save("engine", []byte(`{"v":1}`))
	s.Save("engine", []byte(`{"v":2}`))
	os.Remove(filepath.Join(dir, "engine.json"))

	got, found, err := s.Load("engine")
	if err != nil || !found || string(got) != `{"v":1}` {
		t.Fatalf("%q %v %v", got, found, err)
	}
}

func TestBothVersionsDamagedIsAnError(t *testing.T) {
	s, dir := open(t)
	s.Save("engine", []byte(`{"v":1}`))
	s.Save("engine", []byte(`{"v":2}`))
	os.WriteFile(filepath.Join(dir, "engine.json"), []byte(`garbage`), 0o600)
	os.WriteFile(filepath.Join(dir, "engine.json.prev"), []byte(``), 0o600)

	_, found, err := s.Load("engine")
	if found || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

func TestOnlyADamagedFileAndNoPreviousIsAnError(t *testing.T) {
	s, dir := open(t)
	os.WriteFile(filepath.Join(dir, "engine.json"), []byte(`{`), 0o600)
	if _, _, err := s.Load("engine"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("%v", err)
	}
}

func TestSaveRefusesWhatIsNotJSONAndKeepsTheOldDocument(t *testing.T) {
	s, _ := open(t)
	s.Save("engine", []byte(`{"v":1}`))
	if err := s.Save("engine", []byte(`not json`)); err == nil {
		t.Fatal("want an error")
	}
	got, _, _ := s.Load("engine")
	if string(got) != `{"v":1}` {
		t.Fatalf("the old document must survive: %s", got)
	}
}

func TestDocumentNames(t *testing.T) {
	s, _ := open(t)
	for _, name := range []string{"", ".hidden", "../escape", "a/b", `a\b`} {
		if err := s.Save(name, []byte(`{}`)); err == nil {
			t.Errorf("Save(%q) must be refused", name)
		}
		if _, _, err := s.Load(name); err == nil {
			t.Errorf("Load(%q) must be refused", name)
		}
	}
}

func TestFilesAreNotReadableByOthers(t *testing.T) {
	s, dir := open(t)
	s.Save("engine", []byte(`{}`))
	for _, p := range []string{dir, filepath.Join(dir, "engine.json")} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v: the state holds who armed and when, keep it private", p, info.Mode().Perm())
		}
	}
}

func TestIndependentDocuments(t *testing.T) {
	s, _ := open(t)
	s.Save("engine", []byte(`{"a":1}`))
	s.Save("health", []byte(`{"b":2}`))
	a, _, _ := s.Load("engine")
	b, _, _ := s.Load("health")
	if string(a) != `{"a":1}` || string(b) != `{"b":2}` {
		t.Fatalf("%s %s", a, b)
	}
}

func TestConcurrentSavesLeaveAValidDocument(t *testing.T) {
	s, _ := open(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Save("engine", []byte(`{"v":1}`))
		}()
	}
	wg.Wait()
	if _, found, err := s.Load("engine"); err != nil || !found {
		t.Fatalf("%v %v", found, err)
	}
}

func TestOpenFailsOnAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o600)
	if _, err := Open(filepath.Join(f, "sub"), nil); err == nil {
		t.Fatal("want an error")
	}
}
