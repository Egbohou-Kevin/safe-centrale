package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// updatingFile is the update announcement in the hub's state folder: one line, the
// time the update ends (RFC 3339, UTC). deploy/update-hub.sh writes it before it stops
// anything and removes it once the update is over (docs/operations.md). Its time of
// writing bounds it too: 20 minutes at most (health.MaxUpdating).
const updatingFile = "updating-until"

// readUpdating reads the update announcement of the state folder dir (hub.Deps.Updating):
// until when, and when it was written; both zero without one.
func readUpdating(dir string) func() (until, made time.Time, err error) {
	path := filepath.Join(dir, updatingFile)
	return func() (until, made time.Time, err error) {
		f, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, time.Time{}, nil
		}
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		b, err := io.ReadAll(io.LimitReader(f, 64)) // a time, never more
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		until, err = time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("%s: %w", path, err)
		}
		return until.UTC(), info.ModTime(), nil
	}
}
