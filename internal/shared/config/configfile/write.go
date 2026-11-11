package configfile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// WriteAtomic replaces the file at path with data: it writes a temporary file next
// to it, waits until it is on the disk, then renames it over. A power cut leaves the
// old file or the new one, never half of each.
//
// The file gets the permissions perm and the owner of its folder: a tool run with
// sudo must not leave a file the service that owns the folder cannot read.
func WriteAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // nothing left behind on failure; a no-op after the rename
	if err := fill(tmp, info, data, perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync() // the rename itself reaches the disk
		d.Close()
	}
	return nil
}

// fill writes data to f, owned like the folder dir and with the permissions perm,
// and waits until it is on the disk.
func fill(f *os.File, dir fs.FileInfo, data []byte, perm fs.FileMode) error {
	if err := ownedLike(f, dir); err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// ownedLike gives the new file the owner and group of the folder. Only root can give
// a file away; anyone else already owns what they create in their own folder.
func ownedLike(f *os.File, dir fs.FileInfo) error {
	want, ok := dir.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	mine, err := f.Stat()
	if err != nil {
		return err
	}
	if cur, ok := mine.Sys().(*syscall.Stat_t); ok && cur.Uid == want.Uid && cur.Gid == want.Gid {
		return nil
	}
	if err := f.Chown(int(want.Uid), int(want.Gid)); err != nil {
		return fmt.Errorf("%s belongs to another user: run the tool as that user (sudo -u safehome): %w", filepath.Dir(f.Name()), err)
	}
	return nil
}
