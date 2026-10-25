// Package zigbeeconfig reads the Zigbee adapter's configuration file and its device
// profiles (JSON), strictly: a misspelt key is an error. Each profile is checked
// against the contract when it is read, so that a mistake in a profile stops the
// adapter at start rather than dropping events at night. See
// config/zigbee.example.json and config/profiles/.
package zigbeeconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"safehome/internal/configfile"
	"safehome/internal/contract"
	"safehome/internal/zigbee"
	"safehome/internal/zigbeeapp"
)

// Result is what the files describe.
type Result struct {
	configfile.Program
	App zigbeeapp.Config
}

type file struct {
	configfile.ProgramFile
	BaseTopic    string `json:"base_topic"`
	ProfilesDir  string `json:"profiles_dir"`
	ConfirmWait  string `json:"confirm_wait"`
	Attempts     *int   `json:"attempts"`
	OfflineAfter string `json:"offline_after"`
	PairMax      string `json:"pair_max"`
	Step         *int   `json:"step"`
	StatusEvery  string `json:"status_every"`
}

type ruleFile struct {
	From      string            `json:"from"`
	Map       map[string]string `json:"map"`
	Max       int               `json:"max"`
	Step      int               `json:"step"`
	Momentary bool              `json:"momentary"`
	CodeFrom  string            `json:"code_from"`
	Rest      string            `json:"rest"`
}

type commandFile struct {
	Payload map[string]any `json:"payload"`
	Confirm *struct {
		Capability string `json:"capability"`
		Value      string `json:"value"`
	} `json:"confirm"`
}

type profileFile struct {
	Name         string                 `json:"name"`
	Models       []string               `json:"models"`
	Capabilities map[string]ruleFile    `json:"capabilities"`
	Commands     map[string]commandFile `json:"commands"`
	OfflineAfter string                 `json:"offline_after"`
	Note         string                 `json:"note"` // free text for people: where the profile comes from
}

// Load reads the configuration at path and the profiles it names. A relative
// profiles_dir is taken from the configuration file's folder.
func Load(path string) (Result, error) {
	var f file
	if err := configfile.Decode(path, &f); err != nil {
		return Result{}, fmt.Errorf("zigbeeconfig: %w", err)
	}
	r, err := build(f, filepath.Dir(path))
	if err != nil {
		return Result{}, fmt.Errorf("zigbeeconfig: %s: %w", path, err)
	}
	return r, nil
}

func build(f file, dir string) (Result, error) {
	p, err := f.Build("safehome-zigbee-", "where the adapter keeps its registry")
	if err != nil {
		return Result{}, err
	}
	if f.ProfilesDir == "" {
		return Result{}, errors.New("profiles_dir is required (the folder of the device profiles)")
	}
	r := Result{Program: p}
	if r.App, err = f.app(dir); err != nil {
		return Result{}, err
	}
	return r, nil
}

// app builds the adapter's settings: the defaults, with what the file overrides.
func (f file) app(dir string) (zigbeeapp.Config, error) {
	profiles, err := LoadProfiles(configfile.Path(dir, f.ProfilesDir))
	if err != nil {
		return zigbeeapp.Config{}, err
	}
	z := zigbee.DefaultConfig(profiles)
	if f.Attempts != nil {
		z.Attempts = *f.Attempts
	}
	if f.Step != nil {
		z.Step = *f.Step
	}
	app := zigbeeapp.DefaultConfig(f.Site, z)
	if f.BaseTopic != "" {
		app.Base = f.BaseTopic
	}
	for _, d := range []struct {
		name, value string
		dst         *time.Duration
	}{
		{"confirm_wait", f.ConfirmWait, &app.Zigbee.ConfirmWait},
		{"offline_after", f.OfflineAfter, &app.Zigbee.OfflineAfter},
		{"pair_max", f.PairMax, &app.Zigbee.PairMax},
		{"status_every", f.StatusEvery, &app.StatusEvery},
	} {
		if err := configfile.Duration(d.name, d.value, d.dst); err != nil {
			return zigbeeapp.Config{}, err
		}
	}
	return app, app.Validate()
}

// LoadProfiles reads every *.json file of dir as a profile.
func LoadProfiles(dir string) ([]zigbee.Profile, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no profile in %s: no device could be accepted", dir)
	}
	slices.Sort(paths)
	var out []zigbee.Profile
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		prof, err := ParseProfile(data)
		if err != nil {
			return nil, fmt.Errorf("profile %s: %w", filepath.Base(p), err)
		}
		out = append(out, prof)
	}
	return out, nil
}

// ParseProfile reads one profile and checks it against the contract: every value it
// can produce must be one the contract allows.
func ParseProfile(data []byte) (zigbee.Profile, error) {
	var f profileFile
	if err := configfile.DecodeBytes(data, &f); err != nil {
		return zigbee.Profile{}, err
	}
	p := zigbee.Profile{
		Name: f.Name, Models: f.Models,
		Capabilities: map[string]zigbee.Rule{}, Commands: map[string]zigbee.ProfileCommand{},
	}
	if err := configfile.Duration("offline_after", f.OfflineAfter, &p.OfflineAfter); err != nil {
		return zigbee.Profile{}, err
	}
	for c, rf := range f.Capabilities {
		rule := zigbee.Rule{From: rf.From, Map: rf.Map, Max: rf.Max, Step: rf.Step, Momentary: rf.Momentary, CodeFrom: rf.CodeFrom, Rest: rf.Rest}
		if err := checkCapability(c, rule); err != nil {
			return zigbee.Profile{}, err
		}
		p.Capabilities[c] = rule
	}
	for name, cf := range f.Commands {
		cmd := contract.DeviceCommandData{DeviceID: "x", Command: name, Args: contract.DeviceCommandArgs{DurationS: 1}}
		if err := cmd.Validate(); err != nil {
			return zigbee.Profile{}, fmt.Errorf("command %q is not a device command of the contract", name)
		}
		pc := zigbee.ProfileCommand{Payload: cf.Payload}
		if cf.Confirm != nil {
			pc.Confirm = &zigbee.Expect{Capability: cf.Confirm.Capability, Value: cf.Confirm.Value}
		}
		p.Commands[name] = pc
	}
	if err := (zigbee.Config{Profiles: []zigbee.Profile{p}, ConfirmWait: 1, Attempts: 1, OfflineAfter: 1, PairMax: 1, Step: 1, RestAfter: 1}).Validate(); err != nil {
		return zigbee.Profile{}, err
	}
	return p, nil
}

// checkCapability makes sure a rule only produces values the contract allows. The
// adapter's own capability (availability) and the hub's (mains) are not for
// profiles; a keypad is always an action, and only a keypad carries a code.
func checkCapability(c string, r zigbee.Rule) error {
	switch c {
	case contract.CapAvailability, contract.CapMains:
		return fmt.Errorf("capability %s is not read from a device", c)
	case contract.CapKeypad, contract.CapPanic:
		if !r.Momentary {
			return fmt.Errorf("capability %s is an action: set momentary", c)
		}
	}
	if r.CodeFrom != "" && c != contract.CapKeypad {
		return fmt.Errorf("only a keypad carries a code, not %s", c)
	}
	if len(r.Map) == 0 {
		ev := contract.DeviceEventData{DeviceID: "x", Capability: c, Value: json.RawMessage("0")}
		if err := ev.Validate(); err != nil {
			return fmt.Errorf("capability %s is not a number: give a map of values", c)
		}
		return nil
	}
	for raw, v := range r.Map {
		ev := contract.DeviceEventData{DeviceID: "x", Capability: c, Value: json.RawMessage(strconv.Quote(v))}
		if err := ev.Validate(); err != nil {
			return fmt.Errorf("capability %s: %q maps to %q: %w", c, raw, v, err)
		}
	}
	return nil
}
