package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"safehome/internal/agentconfig"
	"safehome/internal/sealcode"
)

// keyFolders gives the agent's folder of a fresh state directory, not made yet, and that
// state directory, where an earlier agent kept its key.
func keyFolders(t *testing.T) (dir, old string) {
	old = t.TempDir()
	return filepath.Join(old, agentconfig.KeyFolder), old
}

// files lists the names in a folder, a link marked "@", for a test to see what was written.
func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.Type()&os.ModeSymlink != 0 {
			n += "@"
		}
		names = append(names, n)
	}
	return names
}

// writeKey draws a key and writes it at path, as an earlier agent did.
func writeKey(t *testing.T, path string) *sealcode.Key {
	t.Helper()
	k, err := sealcode.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(k.Encode()), 0o600); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestTheCodeKeyIsMadeOnceAndKeptPrivate(t *testing.T) {
	dir, old := keyFolders(t)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	k, err := codeKey(dir, old, log)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "code-key"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	again, err := codeKey(dir, old, log)
	if err != nil || again.Public() != k.Public() {
		t.Fatalf("the key changed at the second start: %v", err)
	}
	if strings.Contains(logs.String(), k.Encode()) {
		t.Fatal("the key is in the log")
	}
}

// A key file opened to others is closed again: whoever reads it opens the codes.
func TestAnOpenCodeKeyIsMadePrivate(t *testing.T) {
	dir, old := keyFolders(t)
	log := slog.New(slog.DiscardHandler)
	k, err := codeKey(dir, old, log)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "code-key")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := codeKey(dir, old, log)
	if err != nil || again.Public() != k.Public() {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

// A key file emptied or cut short (a power cut on the SD card) must not stop the agent,
// which carries the alarms to the cloud: it is set aside, kept, and a new key is drawn,
// whose public half reaches the phones through agent.status.
func TestADamagedCodeKeyDoesNotStopTheAgent(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "cut short": "Ox/TPiG41co6lCIBSL/fgi7F9t", "junk": "%%%\n"} {
		dir, old := keyFolders(t)
		path := filepath.Join(dir, "code-key")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		k, err := codeKey(dir, old, slog.New(slog.NewTextHandler(&logs, nil)))
		if err != nil || k == nil {
			t.Fatalf("%s: %v", name, err)
		}
		if kept, err := os.ReadFile(path + ".damaged"); err != nil || string(kept) != content {
			t.Errorf("%s: the damaged key was not kept aside: %v", name, err)
		}
		if again, err := codeKey(dir, old, slog.New(slog.DiscardHandler)); err != nil || again.Public() != k.Public() {
			t.Errorf("%s: the new key was not kept: %v", name, err)
		}
		if !strings.Contains(logs.String(), "level=ERROR") || (content != "" && strings.Contains(logs.String(), content)) {
			t.Errorf("%s: %s", name, logs.String())
		}
	}
}

// A key that cannot be made private (a file left to root) or whose folder cannot be
// flushed is still used, with a warning: a refusal there would stop the alarms.
func TestAKeyThatCannotBeMadePrivateOrFlushedIsStillUsed(t *testing.T) {
	refuse := func() error { return os.ErrPermission }
	defer func(c func(string, os.FileMode) error, s func(string) error) { chmodFile, syncFolder = c, s }(chmodFile, syncFolder)
	chmodFile = func(string, os.FileMode) error { return refuse() }
	syncFolder = func(string) error { return refuse() }
	dir, old := keyFolders(t)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	k, err := codeKey(dir, old, log)
	if err != nil || k == nil {
		t.Fatalf("a failed folder flush stopped the agent: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "code-key"), 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := codeKey(dir, old, log)
	if err != nil || again.Public() != k.Public() {
		t.Fatalf("a failed chmod stopped the agent: %v", err)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n < 3 || strings.Contains(logs.String(), k.Encode()) {
		t.Fatalf("%d warnings: %s", n, logs.String())
	}
}

// At the first start the agent's folder is made for the owner alone, and the key drawn
// goes into it, none at the old place.
func TestTheKeyGoesIntoTheAgentsOwnFolder(t *testing.T) {
	dir, old := keyFolders(t)
	k, err := codeKey(dir, old, slog.New(slog.DiscardHandler))
	if err != nil || k == nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("the agent's folder: %v %v", st, err)
	}
	if got := files(t, dir); !slices.Equal(got, []string{"code-key"}) {
		t.Fatalf("in the agent's folder: %v", got)
	}
	if got := files(t, old); !slices.Equal(got, []string{agentconfig.KeyFolder}) {
		t.Fatalf("at the old place: %v", got)
	}
}

// A key kept at the old place by an earlier agent is the agent's key: a new one would
// break every seal the phones made for it (bad_seal). It is read there, and nothing is
// written or moved.
func TestAKeyAtTheOldPlaceIsKeptNotRedrawn(t *testing.T) {
	dir, old := keyFolders(t)
	k := writeKey(t, filepath.Join(old, "code-key"))
	var logs bytes.Buffer
	got, err := codeKey(dir, old, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil || got.Public() != k.Public() {
		t.Fatalf("the key at the old place was not used: %v", err)
	}
	if names := files(t, dir); len(names) != 0 {
		t.Fatalf("written into the agent's folder: %v", names)
	}
	if names := files(t, old); !slices.Equal(names, []string{agentconfig.KeyFolder, "code-key"}) {
		t.Fatalf("at the old place: %v", names)
	}
	if !strings.Contains(logs.String(), "old place") || strings.Contains(logs.String(), k.Encode()) {
		t.Fatalf("log: %s", logs.String())
	}
}

// The installer leaves a link at the old place: to the agent's own key, or to wherever
// the key lies. Either way the agent reads the same key, and the link stays.
func TestAKeyReachedThroughALinkIsTheSameKey(t *testing.T) {
	t.Run("to the agent's folder", func(t *testing.T) {
		dir, old := keyFolders(t)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		k := writeKey(t, filepath.Join(dir, "code-key"))
		if err := os.Symlink(filepath.Join(agentconfig.KeyFolder, "code-key"), filepath.Join(old, "code-key")); err != nil {
			t.Fatal(err)
		}
		got, err := codeKey(dir, old, slog.New(slog.DiscardHandler))
		if err != nil || got.Public() != k.Public() {
			t.Fatalf("another key: %v", err)
		}
		if names := files(t, old); !slices.Equal(names, []string{agentconfig.KeyFolder, "code-key@"}) {
			t.Fatalf("at the old place: %v", names)
		}
	})
	t.Run("elsewhere, the agent's folder empty", func(t *testing.T) {
		dir, old := keyFolders(t)
		if err := os.Mkdir(filepath.Join(old, "kept"), 0o700); err != nil {
			t.Fatal(err)
		}
		k := writeKey(t, filepath.Join(old, "kept", "code-key"))
		if err := os.Symlink(filepath.Join("kept", "code-key"), filepath.Join(old, "code-key")); err != nil {
			t.Fatal(err)
		}
		got, err := codeKey(dir, old, slog.New(slog.DiscardHandler))
		if err != nil || got.Public() != k.Public() {
			t.Fatalf("another key: %v", err)
		}
		if names := files(t, dir); len(names) != 0 {
			t.Fatalf("written into the agent's folder: %v", names)
		}
	})
}

// A link at the old place to a key that is gone leaves no key to keep: one is drawn into
// the agent's folder, with a warning.
func TestALinkToNothingDrawsANewKey(t *testing.T) {
	dir, old := keyFolders(t)
	if err := os.Symlink(filepath.Join(agentconfig.KeyFolder, "code-key"), filepath.Join(old, "code-key")); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	k, err := codeKey(dir, old, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil || k == nil {
		t.Fatal(err)
	}
	if got := files(t, dir); !slices.Equal(got, []string{"code-key"}) {
		t.Fatalf("in the agent's folder: %v", got)
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("log: %s", logs.String())
	}
}

// With a key in the agent's folder and another at the old place, the agent's own wins
// and the old one is left as it is.
func TestTheAgentsOwnKeyWinsOverTheOldPlace(t *testing.T) {
	dir, old := keyFolders(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	own := writeKey(t, filepath.Join(dir, "code-key"))
	flat := writeKey(t, filepath.Join(old, "code-key"))
	got, err := codeKey(dir, old, slog.New(slog.DiscardHandler))
	if err != nil || got.Public() != own.Public() || got.Public() == flat.Public() {
		t.Fatalf("the agent's own key lost: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(old, "code-key")); err != nil || string(b) != flat.Encode() {
		t.Fatalf("the old key was touched: %v", err)
	}
}

// A damaged key at the old place is set aside there, and the new one goes into the
// agent's folder.
func TestADamagedKeyAtTheOldPlaceIsSetAsideThere(t *testing.T) {
	dir, old := keyFolders(t)
	path := filepath.Join(old, "code-key")
	if err := os.WriteFile(path, []byte("%%%\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := codeKey(dir, old, slog.New(slog.DiscardHandler))
	if err != nil || k == nil {
		t.Fatal(err)
	}
	if kept, err := os.ReadFile(path + ".damaged"); err != nil || string(kept) != "%%%\n" {
		t.Fatalf("the damaged key was not kept aside: %v", err)
	}
	if got := files(t, dir); !slices.Equal(got, []string{"code-key"}) {
		t.Fatalf("in the agent's folder: %v", got)
	}
	if again, err := codeKey(dir, old, slog.New(slog.DiscardHandler)); err != nil || again.Public() != k.Public() {
		t.Fatalf("the new key was not kept: %v", err)
	}
}
