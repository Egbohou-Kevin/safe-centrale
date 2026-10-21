package hubconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"safehome/internal/configfile"
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
	// Owned like the state folder: a tool run with sudo must not leave a file the hub
	// cannot read.
	if err := configfile.WriteAtomic(filepath.Join(f.StateDir, DevicesFile), data, 0o600); err != nil {
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

// readFile reads the file at path, which must name its state folder.
func readFile(path string) (file, error) {
	f, err := readConfig(path)
	if err != nil {
		return file{}, err
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
	if err := configfile.DecodeBytes(data, &s); err != nil {
		return Site{}, "", false, fmt.Errorf("hubconfig: %s is damaged: %w", DevicesFile, err)
	}
	if s.Zones == nil {
		s.Zones = map[string]Zone{}
	}
	return s, sum(data), true, nil
}
