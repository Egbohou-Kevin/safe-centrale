package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheCodeKeyIsMadeOnceAndKeptPrivate(t *testing.T) {
	dir := t.TempDir()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	k, err := codeKey(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "code-key"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	again, err := codeKey(dir, log)
	if err != nil || again.Public() != k.Public() {
		t.Fatalf("the key changed at the second start: %v", err)
	}
	if strings.Contains(logs.String(), k.Encode()) {
		t.Fatal("the key is in the log")
	}
}

// A key file opened to others is closed again: whoever reads it opens the codes.
func TestAnOpenCodeKeyIsMadePrivate(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	k, err := codeKey(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "code-key")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := codeKey(dir, log)
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
		dir := t.TempDir()
		path := filepath.Join(dir, "code-key")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		k, err := codeKey(dir, slog.New(slog.NewTextHandler(&logs, nil)))
		if err != nil || k == nil {
			t.Fatalf("%s: %v", name, err)
		}
		if kept, err := os.ReadFile(path + ".damaged"); err != nil || string(kept) != content {
			t.Errorf("%s: the damaged key was not kept aside: %v", name, err)
		}
		if again, err := codeKey(dir, slog.New(slog.DiscardHandler)); err != nil || again.Public() != k.Public() {
			t.Errorf("%s: the new key was not kept: %v", name, err)
		}
		if !strings.Contains(logs.String(), "level=ERROR") || (content != "" && strings.Contains(logs.String(), content)) {
			t.Errorf("%s: %s", name, logs.String())
		}
	}
}
