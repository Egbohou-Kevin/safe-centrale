package hubconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// DevicesFile is the file of the hub's state folder that holds the zones and the
// sirens set by the installer's tools (the command-line tool, the local console). It
// replaces those of the configuration file: /etc/safehome is read-only on a running
// hub, and the devices change after the installation (a sensor added, a siren moved).
const DevicesFile = "site-devices.json"

// Zone is what a device is for.
type Zone struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Site is what the installer's tools change: the zones and the sirens. Everything
// else (contacts, delays...) is written by hand or sent by the cloud.
type Site struct {
	Zones    map[string]Zone `json:"zones"`
	SirenIDs []string        `json:"siren_ids"`
}

// ReadSite returns the zones and the sirens the hub at path runs with: those of the
// state folder once a tool wrote them, those of the configuration file before.
func ReadSite(path string) (Site, error) {
	f, err := readFile(path)
	if err != nil {
		return Site{}, err
	}
	s := Site{Zones: map[string]Zone{}, SirenIDs: f.SirenIDs}
	for id, z := range f.Zones {
		s.Zones[id] = Zone(z)
	}
	d, _, found, err := readDevices(f.StateDir)
	switch {
	case err != nil:
		return Site{}, err
	case found:
		return d, nil
	}
	return s, nil
}

// WriteSite keeps the zones and the sirens in the state folder of the hub at path,
// once the hub's whole configuration is checked with them. The file is replaced
// atomically, readable by the hub's user only. The hub reads it at its next start.
func WriteSite(path string, s Site) error {
	f, err := readFile(path)
	if err != nil {
		return err
	}
	data, err := checkSite(f, s)
	if err != nil {
		return err
	}
	if err := writeDevices(f.StateDir, data); err != nil {
		return fmt.Errorf("hubconfig: %w", err)
	}
	return nil
}

// CheckSite says whether WriteSite would accept s, without writing anything: a tool
// asks before an action that cannot be undone (removing a device from the network).
func CheckSite(path string, s Site) error {
	f, err := readFile(path)
	if err != nil {
		return err
	}
	_, err = checkSite(f, s)
	return err
}

// DevicesSum identifies the content of the devices file of a state folder, empty
// when there is none: the hub compares it with the one it loaded, to know whether to
// restart.
func DevicesSum(stateDir string) string {
	data, err := os.ReadFile(filepath.Join(stateDir, DevicesFile))
	if err != nil {
		return ""
	}
	return sum(data)
}

func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func readFile(path string) (file, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return file{}, fmt.Errorf("hubconfig: %w", err)
	}
	f, err := decode(data)
	if err != nil {
		return file{}, fmt.Errorf("hubconfig: %s: %w", path, err)
	}
	if f.StateDir == "" {
		return file{}, fmt.Errorf("hubconfig: %s: state_dir is required", path)
	}
	return f, nil
}

// checkSite returns the devices file for s, once the configuration builds with it.
func checkSite(f file, s Site) ([]byte, error) {
	if s.Zones == nil {
		s.Zones = map[string]Zone{}
	}
	if s.SirenIDs == nil {
		s.SirenIDs = []string{}
	}
	withDevices(&f, s)
	if _, err := build(f); err != nil {
		return nil, fmt.Errorf("hubconfig: the hub would refuse these zones and sirens: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("hubconfig: %w", err)
	}
	return append(data, '\n'), nil
}

func withDevices(f *file, s Site) {
	f.Zones = make(map[string]zoneFile, len(s.Zones))
	for id, z := range s.Zones {
		f.Zones[id] = zoneFile(z)
	}
	f.SirenIDs = s.SirenIDs
}

// readDevices reads the devices file of a state folder, strictly.
func readDevices(dir string) (s Site, digest string, found bool, err error) {
	data, err := os.ReadFile(filepath.Join(dir, DevicesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Site{}, "", false, nil
	}
	if err != nil {
		return Site{}, "", false, fmt.Errorf("hubconfig: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil || dec.More() {
		return Site{}, "", false, fmt.Errorf("hubconfig: %s is damaged: %v", DevicesFile, err)
	}
	if s.Zones == nil {
		s.Zones = map[string]Zone{}
	}
	return s, sum(data), true, nil
}

// writeDevices writes data next to the devices file, then renames it over: a power
// cut leaves the old file or the new one, never half of each. The file belongs to
// the owner of the state folder: a tool run with sudo must not leave a file the hub
// cannot read.
func writeDevices(dir string, data []byte) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+DevicesFile+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // nothing left behind on failure; a no-op after the rename
	if err := ownedLike(tmp, info); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, DevicesFile)); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync() // the rename itself reaches the disk
		d.Close()
	}
	return nil
}

// ownedLike gives the new file the owner and group of the folder. Only root can give
// a file away; anyone else already owns what they create in their own folder.
func ownedLike(f *os.File, dir os.FileInfo) error {
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
		return fmt.Errorf("the state folder belongs to another user: run the tool as that user (sudo -u safehome): %w", err)
	}
	return nil
}
