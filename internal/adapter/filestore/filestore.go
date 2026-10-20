// Package filestore keeps small JSON documents on disk, such as the engine's
// snapshot, and survives a power cut at any moment (the hub runs on a Raspberry Pi
// and a SD card).
//
// A write goes to a temporary file, is flushed to the disk, and only then replaces
// the real file with an atomic rename, so a reader sees the old document or the new
// one, never half of one. The previous version is kept next to it (name.json.prev)
// in case the current one is damaged anyway.
package filestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// ErrCorrupt means a document exists but neither it nor its previous version holds
// valid JSON. The caller decides what to do: for an alarm, silently starting from
// scratch is not a safe default.
var ErrCorrupt = errors.New("filestore: document is corrupt")

// Store is a directory of JSON documents.
type Store struct {
	dir string
	log *slog.Logger
}

// Open creates the directory if needed and removes the temporary files that a crash
// may have left behind. A nil logger discards the logs.
func Open(dir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("filestore: %w", err)
	}
	stale, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	for _, f := range stale {
		os.Remove(f)
	}
	return &Store{dir: dir, log: log}, nil
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name+".json") }

func checkName(name string) error {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("filestore: invalid document name %q", name)
	}
	return nil
}

// Save replaces the document name with data, which must be valid JSON.
func (s *Store) Save(name string, data []byte) error {
	if err := checkName(name); err != nil {
		return err
	}
	if !json.Valid(data) {
		return fmt.Errorf("filestore: %s: data is not valid JSON", name)
	}

	tmp, err := os.CreateTemp(s.dir, name+".*.tmp")
	if err != nil {
		return fmt.Errorf("filestore: %w", err)
	}
	cleanup := func() { tmp.Close(); os.Remove(tmp.Name()) }
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("filestore: %w", err)
	}
	if err := tmp.Sync(); err != nil { // on the disk, not only in the system cache
		cleanup()
		return fmt.Errorf("filestore: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("filestore: %w", err)
	}

	// Keep the current version as the previous one, with a hard link so the current
	// file never disappears. It is best effort: it only matters if the next file
	// turns out damaged.
	final := s.path(name)
	prev := final + ".prev"
	if _, err := os.Stat(final); err == nil {
		os.Remove(prev)
		if err := os.Link(final, prev); err != nil {
			s.log.Warn("filestore: cannot keep the previous version", "name", name, "err", err)
		}
	}

	if err := os.Rename(tmp.Name(), final); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("filestore: %w", err)
	}
	return syncDir(s.dir)
}

// syncDir flushes the directory entry, so the rename itself survives a power cut.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("filestore: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("filestore: %w", err)
	}
	return nil
}

// Load returns the document name. found is false if it never existed. If the
// current file is damaged, the previous version is returned instead (and a warning
// is logged); if both are damaged the error wraps ErrCorrupt.
func (s *Store) Load(name string) (data []byte, found bool, err error) {
	if err := checkName(name); err != nil {
		return nil, false, err
	}
	main, mainErr := s.read(s.path(name))
	if mainErr == nil {
		return main, true, nil
	}
	prev, prevErr := s.read(s.path(name) + ".prev")
	if prevErr == nil {
		s.log.Warn("filestore: current document unusable, using the previous version", "name", name, "err", mainErr)
		return prev, true, nil
	}
	if errors.Is(mainErr, fs.ErrNotExist) && errors.Is(prevErr, fs.ErrNotExist) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("%w: %s (current: %v; previous: %v)", ErrCorrupt, name, mainErr, prevErr)
}

func (s *Store) read(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, errors.New("not valid JSON")
	}
	return b, nil
}
