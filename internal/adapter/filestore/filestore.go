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
	"syscall"
	"time"
)

// ErrCorrupt means a document exists but neither it nor its previous version holds
// valid JSON. The caller decides what to do: for an alarm, silently starting from
// scratch is not a safe default.
var ErrCorrupt = errors.New("filestore: document is corrupt")

// Store is a directory of JSON documents.
type Store struct {
	dir   string
	log   *slog.Logger
	owner *syscall.Stat_t // the folder's owner and group; nil where the system has none
	old   string          // where the documents were kept before (see OpenMoved); "" for none
}

// staleAfter is the age from which a temporary file is a crash's leftover. Several
// programs open the same directory (the hub, and the console or a command-line tool
// reading the codes): a younger one may be another process's save under way.
const staleAfter = 10 * time.Minute

// Open creates the directory if needed and removes the temporary files that a crash
// may have left behind. A nil logger discards the logs.
func Open(dir string, log *slog.Logger) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("filestore: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("filestore: %w", err)
	}
	removeStale(dir, time.Now().Add(-staleAfter))
	return newStore(dir, log, info), nil
}

// OpenMoved is Open for documents that move from the folder old to dir: a document
// dir does not hold yet is read in old, so a program never starts without what it
// kept there (the installation copies them, the program moves nothing). Every write
// goes to dir. A dir made here gets old's owner and group, as the documents do: made
// by a tool run with plain sudo, root's alone, the service that owns old could never
// read it.
func OpenMoved(dir, old string, log *slog.Logger) (*Store, error) {
	if err := os.MkdirAll(old, 0o700); err != nil {
		return nil, fmt.Errorf("filestore: %w", err)
	}
	info, err := os.Stat(old)
	if err != nil {
		return nil, fmt.Errorf("filestore: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, fmt.Errorf("filestore: %w", err)
	}
	switch err := os.Mkdir(dir, 0o700); {
	case err == nil:
		if err := ownedLike(dir, info); err != nil {
			os.Remove(dir)
			return nil, err
		}
	case !errors.Is(err, fs.ErrExist):
		return nil, fmt.Errorf("filestore: %w", err)
	}
	s, err := Open(dir, log)
	if err != nil {
		return nil, err
	}
	s.old = old
	return s, nil
}

// ownedLike gives the folder dir the owner and group of info, unless it has them.
func ownedLike(dir string, info fs.FileInfo) error {
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	cur, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("filestore: %w", err)
	}
	if st, ok := cur.Sys().(*syscall.Stat_t); ok && st.Uid == owner.Uid && st.Gid == owner.Gid {
		return nil
	}
	if err := os.Chown(dir, int(owner.Uid), int(owner.Gid)); err != nil {
		return fmt.Errorf("filestore: %s belongs to another user: run the tool as that user (sudo -u safehome): %w", filepath.Dir(dir), err)
	}
	return nil
}

func newStore(dir string, log *slog.Logger, info fs.FileInfo) *Store {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	owner, _ := info.Sys().(*syscall.Stat_t)
	return &Store{dir: dir, log: log, owner: owner}
}

// removeStale removes the temporary files last written before limit.
func removeStale(dir string, limit time.Time) {
	tmps, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	for _, f := range tmps {
		if info, err := os.Stat(f); err == nil && info.ModTime().Before(limit) {
			os.Remove(f)
		}
	}
}

func (s *Store) path(name string) string { return pathIn(s.dir, name) }

func pathIn(dir, name string) string { return filepath.Join(dir, name+".json") }

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
	if err := s.ownedLikeTheFolder(tmp); err != nil {
		cleanup()
		return err
	}
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

// ownedLikeTheFolder gives a new file the owner and group of the folder: a tool run
// with sudo must not leave a document the service that owns the folder cannot read
// (the same rule as the configuration files'; an adapter shares no code). Only root
// can give a file away; anyone else already owns what they create in their own folder.
func (s *Store) ownedLikeTheFolder(f *os.File) error {
	if s.owner == nil {
		return nil
	}
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("filestore: %w", err)
	}
	if cur, ok := info.Sys().(*syscall.Stat_t); ok && cur.Uid == s.owner.Uid && cur.Gid == s.owner.Gid {
		return nil
	}
	if err := f.Chown(int(s.owner.Uid), int(s.owner.Gid)); err != nil {
		return fmt.Errorf("filestore: %s belongs to another user: run the tool as that user (sudo -u safehome): %w", s.dir, err)
	}
	return nil
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
// is logged); if both are damaged the error wraps ErrCorrupt. A document the folder
// does not hold is read where it was kept before (see OpenMoved); a damaged one never
// is: an older copy elsewhere is not the previous version.
func (s *Store) Load(name string) (data []byte, found bool, err error) {
	if err := checkName(name); err != nil {
		return nil, false, err
	}
	data, found, err = s.load(s.dir, name)
	if err != nil || found || s.old == "" {
		return data, found, err
	}
	data, found, err = s.load(s.old, name)
	if found {
		s.log.Info("filestore: document read where it was kept before, until the installation moves it", "name", name, "from", s.old)
	}
	return data, found, err
}

// load reads the document name in dir, or its previous version.
func (s *Store) load(dir, name string) (data []byte, found bool, err error) {
	main, mainErr := s.read(pathIn(dir, name))
	if mainErr == nil {
		return main, true, nil
	}
	prev, prevErr := s.read(pathIn(dir, name) + ".prev")
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
