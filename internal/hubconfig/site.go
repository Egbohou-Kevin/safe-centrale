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
	"safehome/internal/install"
)

// DevicesFile is the file of the hub's state folder that holds the zones and the
// sirens set by the installer's tools (the command-line tool, the local console). It
// replaces those of the configuration file: /etc/safehome is read-only on a running
// hub, and the devices change after the installation (a sensor added, a siren moved).
const DevicesFile = "site-devices.json"

// devicesFile is the form of DevicesFile: what the installer's tools change, the zones
// and the sirens (install.Site). Everything else (contacts, delays...) is written by
// hand or sent by the cloud.
type devicesFile struct {
	Zones    map[string]zoneFile `json:"zones"`
	SirenIDs []string            `json:"siren_ids"`
}

// SiteFiles gives the installer's tools (install.Sites) the zones and the sirens of
// the hub whose configuration file is at Path.
type SiteFiles struct{ Path string }

func (s SiteFiles) Read() (install.Site, error)   { return ReadSite(s.Path) }
func (s SiteFiles) Check(site install.Site) error { return CheckSite(s.Path, site) }
func (s SiteFiles) Write(site install.Site) error { return WriteSite(s.Path, site) }

// ReadSite returns the zones and the sirens the hub at path runs with: those of the
// state folder once a tool wrote them, those of the configuration file before.
func ReadSite(path string) (install.Site, error) {
	f, err := readFile(path)
	if err != nil {
		return install.Site{}, err
	}
	d, _, found, err := readDevices(f.StateDir)
	switch {
	case err != nil:
		return install.Site{}, err
	case found:
		return siteOf(d.Zones, d.SirenIDs), nil
	}
	return siteOf(f.Zones, f.SirenIDs), nil
}

func siteOf(zones map[string]zoneFile, sirenIDs []string) install.Site {
	s := install.Site{Zones: make(map[string]install.Zone, len(zones)), SirenIDs: sirenIDs}
	for id, z := range zones {
		s.Zones[id] = install.Zone(z)
	}
	return s
}

// WriteSite keeps the zones and the sirens in the state folder of the hub at path,
// once the hub's whole configuration is checked with them. The file is replaced
// atomically, readable by the hub's user only. The hub reads it at its next start.
func WriteSite(path string, s install.Site) error {
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
func CheckSite(path string, s install.Site) error {
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
func checkSite(f file, s install.Site) ([]byte, error) {
	d := devicesFile{Zones: make(map[string]zoneFile, len(s.Zones)), SirenIDs: s.SirenIDs}
	for id, z := range s.Zones {
		d.Zones[id] = zoneFile(z)
	}
	if d.SirenIDs == nil {
		d.SirenIDs = []string{}
	}
	withDevices(&f, d)
	if _, err := build(f); err != nil {
		return nil, fmt.Errorf("hubconfig: the hub would refuse these zones and sirens: %w", err)
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("hubconfig: %w", err)
	}
	return append(data, '\n'), nil
}

// withDevices lays the devices file d over f.
func withDevices(f *file, d devicesFile) {
	f.Zones, f.SirenIDs = d.Zones, d.SirenIDs
}

// readDevices reads the devices file of a state folder, strictly.
func readDevices(dir string) (d devicesFile, digest string, found bool, err error) {
	data, err := os.ReadFile(filepath.Join(dir, DevicesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return devicesFile{}, "", false, nil
	}
	if err != nil {
		return devicesFile{}, "", false, fmt.Errorf("hubconfig: %w", err)
	}
	if err := configfile.DecodeBytes(data, &d); err != nil {
		return devicesFile{}, "", false, fmt.Errorf("hubconfig: %s is damaged: %w", DevicesFile, err)
	}
	if d.Zones == nil {
		d.Zones = map[string]zoneFile{}
	}
	return d, sum(data), true, nil
}
