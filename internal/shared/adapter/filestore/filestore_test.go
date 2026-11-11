package filestore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
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
	old := time.Now().Add(-time.Hour)
	os.Chtimes(stale, old, old)
	if _, err := Open(dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("the stale temporary file must be removed")
	}
}

// Another program opening the same directory (the console reading the codes while
// the hub saves its snapshot) must not remove a save under way.
func TestOpenLeavesASaveUnderWay(t *testing.T) {
	_, dir := open(t)
	writing := filepath.Join(dir, "engine.67890.tmp")
	os.WriteFile(writing, []byte(`{"ha`), 0o600)
	if _, err := Open(dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(writing); err != nil {
		t.Fatalf("another process's save was removed: %v", err)
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

// A tool run as another user than the folder's owner (sudo without -u safehome) must
// not leave a document the hub cannot read: the document gets the folder's owner, or
// the save fails and says why. Only root can give a file away, so here, as an
// ordinary user in a folder of root's that anyone may write to (/tmp), it must fail.
func TestADocumentGetsTheFoldersOwnerOrIsNotSaved(t *testing.T) {
	dir, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Skip("no /tmp")
	}
	info, err := os.Stat(dir)
	if err != nil || os.Geteuid() == 0 || info.Mode().Perm()&0o002 == 0 || ownerOf(info) == os.Geteuid() {
		t.Skip("needs a world-writable /tmp owned by someone else, and not running as root")
	}
	s := newStore(dir, nil, info)
	name := fmt.Sprintf("safehome-filestore-test-%d", os.Getpid())
	t.Cleanup(func() { os.Remove(s.path(name)) })
	err = s.Save(name, []byte(`{"v":1}`))
	if err == nil || !strings.Contains(err.Error(), "run the tool as that user") {
		t.Fatalf("a document owned by the wrong user was saved: %v", err)
	}
	if _, err := os.Stat(s.path(name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the document is there: %v", err)
	}
	if tmps, _ := filepath.Glob(filepath.Join(dir, name+".*.tmp")); len(tmps) != 0 {
		t.Fatalf("left behind: %v", tmps)
	}
}

func ownerOf(info os.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

func TestAMovedDocumentIsReadWhereItWasKeptUntilTheFolderHoldsIt(t *testing.T) {
	old := filepath.Join(t.TempDir(), "state")
	flat, err := Open(old, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := flat.Save("access", []byte(`["flat"]`)); err != nil {
		t.Fatal(err)
	}
	if err := flat.Save("installer", []byte(`["flat"]`)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(old, "codes")
	s, err := OpenMoved(dir, old, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("the folder: %v %v", info, err)
	}
	if got, found, err := s.Load("access"); err != nil || !found || string(got) != `["flat"]` {
		t.Fatalf("not yet moved: %q %v %v", got, found, err)
	}

	// A write goes to the folder, which then wins over the old place, document by document.
	if err := s.Save("access", []byte(`["moved"]`)); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(old, "access.json")); err != nil || string(b) != `["flat"]` {
		t.Fatalf("the old place is left as it was: %q %v", b, err)
	}
	if got, _, _ := s.Load("access"); string(got) != `["moved"]` {
		t.Fatalf("both places: %q", got)
	}
	if got, _, _ := s.Load("installer"); string(got) != `["flat"]` {
		t.Fatalf("another document still at the old place: %q", got)
	}
	if _, found, err := s.Load("codes-taken"); found || err != nil {
		t.Fatalf("in neither place: %v %v", found, err)
	}

	// The old place left as a link to the folder (what the installation does) reads the same.
	if err := os.Remove(filepath.Join(old, "access.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("codes/access.json", filepath.Join(old, "access.json")); err != nil {
		t.Fatal(err)
	}
	again, err := OpenMoved(dir, old, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := again.Load("access"); string(got) != `["moved"]` {
		t.Fatalf("through the link: %q", got)
	}
}

func TestADamagedMovedDocumentIsNeverReadWhereItWasKept(t *testing.T) {
	old := t.TempDir()
	if err := os.WriteFile(filepath.Join(old, "access.json"), []byte(`["flat"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(old, "codes")
	s, err := OpenMoved(dir, old, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "access.json"), []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Load("access"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("an older copy is not the previous version: %v", err)
	}
}

func TestAMovedFolderGetsTheOwnerOfTheOldOne(t *testing.T) {
	old := t.TempDir()
	dir := filepath.Join(old, "codes")
	if _, err := OpenMoved(dir, old, nil); err != nil {
		t.Fatal(err)
	}
	o, _ := os.Stat(old)
	d, _ := os.Stat(dir)
	if ownerOf(o) != ownerOf(d) {
		t.Fatalf("%d, not %d", ownerOf(d), ownerOf(o))
	}
	if os.Getuid() == 0 {
		t.Skip("root may give a folder away")
	}
	// A folder to give to another user is refused to whoever is not root.
	if err := ownedLike(dir, fakeOwner{o}); err == nil {
		t.Fatal("a folder given away by a user who is not root")
	}
}

// fakeOwner is the information of a folder owned by root.
type fakeOwner struct{ os.FileInfo }

func (fakeOwner) Sys() any { return &syscall.Stat_t{Uid: 0, Gid: 0} }
