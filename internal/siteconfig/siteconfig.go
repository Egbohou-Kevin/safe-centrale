// Package siteconfig keeps the configuration the cloud set (config.set) in the
// hub's state folder, next to the programs' state: /etc/safehome is read-only on a
// running hub. Each program reads its own files, then lays this configuration over
// them when it starts (hubconfig, gsmconfig, videoconfig). The hub writes it, after
// checking it, and restarts to apply it.
//
// The file holds phone numbers: it is written for the service's user only, and
// nothing here prints it.
package siteconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"safehome/internal/contract"
)

// FileName is the file in the state folder.
const FileName = "site-config.json"

// Read returns the configuration set from the cloud, if any.
func Read(dir string) (contract.ConfigSetData, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, FileName))
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
		return contract.ConfigSetData{}, false, fmt.Errorf("siteconfig: %s: %w", FileName, err)
	}
	return d, true, nil
}

// Write replaces the configuration, atomically: after a power cut the file is the
// old one or the new one, never half of each.
func Write(dir string, d contract.ConfigSetData) error {
	if err := d.Validate(); err != nil {
		return fmt.Errorf("siteconfig: %w", err)
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("siteconfig: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+FileName+"-*")
	if err != nil {
		return fmt.Errorf("siteconfig: %w", err)
	}
	defer os.Remove(tmp.Name()) // gone once renamed
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("siteconfig: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, FileName)); err != nil {
		return fmt.Errorf("siteconfig: %w", err)
	}
	if f, err := os.Open(dir); err == nil {
		f.Sync() // the rename itself reaches the disk
		f.Close()
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
