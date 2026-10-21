// Package queuedir keeps a queue on disk, one small JSON file per item, and survives
// a power cut at any moment.
//
// One file per item rather than one file for the whole queue: the link agent's queue
// may hold 20000 items and 10 MB, and rewriting it for every item sent would write
// about 100 GB on the SD card to drain it once. Rather than an append-only log too:
// the file system already does the bookkeeping a log would need (framing, checksums
// of torn records, compaction), and a lost item is one file, not the rest of a log.
//
// A write goes to a temporary file, is flushed to the disk, then takes the item's
// name with an atomic rename, so an item is whole or absent. A removal is not
// flushed on its own, which spares a write per item sent: a power cut may bring a
// removed item back, and the agent sends it again (the cloud drops duplicates by id).
package queuedir

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	ext    = ".json"
	tmpExt = ".tmp"
)

// Dir is a directory of items. It belongs to one process (the agent holds a lock
// on its state directory), so whatever temporary file it finds at Open is a
// leftover.
type Dir struct {
	dir string
	log *slog.Logger
}

// Open creates the directory if needed and removes what a crash may have left: the
// temporary files. A nil logger discards the logs.
func Open(dir string, log *slog.Logger) (*Dir, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("queuedir: %w", err)
	}
	tmps, err := filepath.Glob(filepath.Join(dir, "*"+tmpExt))
	if err != nil {
		return nil, fmt.Errorf("queuedir: %w", err)
	}
	for _, f := range tmps {
		os.Remove(f)
	}
	return &Dir{dir: dir, log: log}, nil
}

func checkName(name string) error {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("queuedir: invalid item name %q", name)
	}
	return nil
}

// Put writes the item name, replacing it if it exists. data must be valid JSON.
func (d *Dir) Put(name string, data []byte) error {
	if err := checkName(name); err != nil {
		return err
	}
	if !json.Valid(data) {
		return fmt.Errorf("queuedir: %s: data is not valid JSON", name)
	}
	tmp, err := os.CreateTemp(d.dir, name+".*"+tmpExt)
	if err != nil {
		return fmt.Errorf("queuedir: %w", err)
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync() // on the disk, not only in the system cache
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(d.dir, name+ext))
	}
	if err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("queuedir: %w", err)
	}
	return d.syncDir()
}

// syncDir flushes the directory, so the rename itself survives a power cut.
func (d *Dir) syncDir() error {
	f, err := os.Open(d.dir)
	if err != nil {
		return fmt.Errorf("queuedir: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("queuedir: %w", err)
	}
	return nil
}

// Remove removes the items named; one that does not exist is no error.
func (d *Dir) Remove(names ...string) error {
	for _, name := range names {
		if err := checkName(name); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(d.dir, name+ext)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("queuedir: %w", err)
		}
	}
	return nil
}

// Load returns every item, by name. A file that is not valid JSON cannot be a whole
// item (writes are atomic): the file system lost it in a crash, and it is removed
// with a warning.
func (d *Dir) Load() (map[string][]byte, error) {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return nil, fmt.Errorf("queuedir: %w", err)
	}
	items := map[string][]byte{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ext)
		if !ok || !e.Type().IsRegular() || checkName(name) != nil {
			continue
		}
		path := filepath.Join(d.dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("queuedir: %w", err)
		}
		if !json.Valid(data) {
			d.log.Warn("queuedir: removing a damaged item", "name", name, "bytes", len(data))
			os.Remove(path)
			continue
		}
		items[name] = data
	}
	return items, nil
}
