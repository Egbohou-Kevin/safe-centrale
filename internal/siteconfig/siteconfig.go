// Package siteconfig keeps the configuration the cloud set (config.set) in the
// hub's state folder, next to the programs' state: /etc/safehome is read-only on a
// running hub. Each program reads its own files, then lays this configuration over
// them when it starts (hubconfig, gsmconfig, videoconfig). The hub writes it, after
// checking it, and restarts to apply it.
//
// The file holds phone numbers: it is written for the service's user only, and
// nothing here prints it.
//
// It lives in a folder of its own, site/ (28 October 2026): the video service, which
// runs third-party code, sees that folder and not the rest of the state (code key,
// access codes). A folder, not the file alone: the file is replaced by rename, and a
// file bound into a service's view would stay the old one. A copy is still kept at the
// state folder's top, where the programs before that date read it: a program put back
// by update-hub.sh's rollback runs on the latest configuration too.
package siteconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"safehome/internal/configfile"
	"safehome/internal/contract"
)

// FileName is the file, in Dir and, for the programs before 28 October 2026, at the
// state folder's top.
const FileName = "site-config.json"

// Dir is the file's folder in the state folder; the video service sees it alone.
const Dir = "site"

// Read returns the configuration set from the cloud, if any. A file that reads but no
// longer fits the contract (an update made it stricter) comes back with its version
// alone, found, and the error: the hub refuses that version to the cloud by its
// number, instead of leaving the cloud believing it applied.
//
// Of the two copies the higher version wins, Dir's on a tie: a program put back by a
// rollback wrote the top one alone, and a hub updated before its first configuration
// holds no other. The video service sees Dir's alone (install.sh fills it).
func Read(dir string) (contract.ConfigSetData, bool, error) {
	d, found, err := readFile(filepath.Join(dir, Dir, FileName))
	top, topFound, topErr := readFile(filepath.Join(dir, FileName))
	if !topFound && topErr == nil {
		return d, found, err
	}
	if (found || err != nil) && d.Version >= top.Version {
		return d, found, err
	}
	return top, topFound, topErr
}

func readFile(path string) (contract.ConfigSetData, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return contract.ConfigSetData{}, false, nil
	}
	if err != nil {
		return contract.ConfigSetData{}, false, fmt.Errorf("siteconfig: %w", err)
	}
	var d contract.ConfigSetData
	if err := json.Unmarshal(data, &d); err != nil {
		return contract.ConfigSetData{}, false, fmt.Errorf("siteconfig: %s is damaged: %w", FileName, err)
	}
	if err := d.Validate(); err != nil {
		return contract.ConfigSetData{Version: max(d.Version, 0)}, true, fmt.Errorf("siteconfig: %s: %w", FileName, err)
	}
	return d, true, nil
}

// Write replaces the configuration, atomically: after a power cut each copy is the
// old one or the new one, never half of each. Dir's first, then the top one. Only the
// state folder's owner may read them; a missing state folder is an error.
func Write(dir string, d contract.ConfigSetData) error {
	if err := d.Validate(); err != nil {
		return fmt.Errorf("siteconfig: %w", err)
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("siteconfig: %w", err)
	}
	if err := os.Mkdir(filepath.Join(dir, Dir), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("siteconfig: %w", err)
	}
	for _, path := range []string{filepath.Join(dir, Dir, FileName), filepath.Join(dir, FileName)} {
		if err := configfile.WriteAtomic(path, data, 0o600); err != nil {
			return fmt.Errorf("siteconfig: %w", err)
		}
	}
	return nil
}

// Changed tells a program that lays this configuration over its files (the GSM
// gateway, the video service) whether to restart, given a message received on
// config/applied: the hub runs with a new version, and the file now holds another
// version than the one the program loaded (running, 0 for none).
//
// The file decides, not the message: config.applied is retained, and may name an
// older version than a file the hub kept already and applies at its next quiet
// moment. A file that cannot be read never makes a program restart: it would restart
// again and again.
func Changed(dir, site string, running int, payload []byte) (int, bool) {
	env, err := contract.ParseEnvelope(payload)
	if err != nil || env.Site != site || env.Type != contract.TypeConfigApplied {
		return 0, false
	}
	a, err := contract.DecodeData[contract.ConfigAppliedData](env)
	if err != nil || a.Status != contract.ConfigApplied || a.Version == running {
		return 0, false
	}
	d, ok, err := Read(dir)
	if err != nil || !ok || d.Version == running {
		return 0, false
	}
	return d.Version, true
}
