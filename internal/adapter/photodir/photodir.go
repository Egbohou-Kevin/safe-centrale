// Package photodir keeps the photos taken at an alarm in a folder of the state
// directory, until the link agent has sent them: the video service writes them, the
// agent reads and removes them. A photo is one JPEG file named after its id.
//
// The folder is bounded: past its limits the oldest photos go first. The SD card is
// spared too: a photo is written once, then read once.
package photodir

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Dir is the photos' folder.
type Dir struct {
	path     string
	maxFiles int
	maxBytes int64
}

const ext = ".jpg"

// ErrNotFound says there is no such photo (sent already, or pushed out).
var ErrNotFound = errors.New("photodir: no such photo")

// The ids are the contract's ULIDs; nothing else becomes a file name.
var validID = regexp.MustCompile(`^[0-9A-Za-z]{1,40}$`)

// Open creates the folder if needed, readable by the service only.
func Open(path string, maxFiles int, maxBytes int64) (*Dir, error) {
	if maxFiles < 1 || maxBytes < 1 {
		return nil, errors.New("photodir: the limits must be positive")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("photodir: %w", err)
	}
	return &Dir{path: path, maxFiles: maxFiles, maxBytes: maxBytes}, nil
}

func (d *Dir) file(id string) (string, error) {
	if !validID.MatchString(id) {
		return "", fmt.Errorf("photodir: invalid photo id %q", id)
	}
	return filepath.Join(d.path, id+ext), nil
}

// Save writes a photo, whole or not at all, then makes room by removing the oldest
// ones beyond the limits.
func (d *Dir) Save(id string, jpeg []byte) error {
	name, err := d.file(id)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d.path, ".photo-*")
	if err != nil {
		return fmt.Errorf("photodir: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(jpeg)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), name)
	}
	if err != nil {
		return fmt.Errorf("photodir: %w", err)
	}
	return d.prune(id)
}

// prune removes the oldest photos beyond the limits, never the one just saved.
func (d *Dir) prune(keep string) error {
	entries, err := os.ReadDir(d.path)
	if err != nil {
		return fmt.Errorf("photodir: %w", err)
	}
	type photo struct {
		name string
		size int64
		mod  int64
	}
	var all []photo
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		all = append(all, photo{e.Name(), info.Size(), info.ModTime().UnixNano()})
		total += info.Size()
	}
	// Oldest first; the ids are ULIDs, which sort by time, for photos of the same instant.
	slices.SortFunc(all, func(a, b photo) int { return cmp.Or(cmp.Compare(a.mod, b.mod), strings.Compare(a.name, b.name)) })
	count := len(all)
	for _, p := range all {
		if count <= d.maxFiles && total <= d.maxBytes {
			break
		}
		if p.name == keep+ext {
			continue
		}
		if err := os.Remove(filepath.Join(d.path, p.name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("photodir: %w", err)
		}
		count, total = count-1, total-p.size
	}
	return nil
}

// Open returns a photo and its size. ErrNotFound says it is gone.
func (d *Dir) Open(id string) (io.ReadCloser, int64, error) {
	name, err := d.file(id)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("photodir: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("photodir: %w", err)
	}
	return f, info.Size(), nil
}

// Remove deletes a photo; one already gone is fine.
func (d *Dir) Remove(id string) error {
	name, err := d.file(id)
	if err != nil {
		return err
	}
	if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("photodir: %w", err)
	}
	return nil
}
