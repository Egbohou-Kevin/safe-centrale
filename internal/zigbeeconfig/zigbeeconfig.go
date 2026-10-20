// Package zigbeeconfig reads the Zigbee adapter's configuration file and its device
// profiles (JSON), strictly: a misspelt key is an error. Each profile is checked
// against the contract when it is read, so that a mistake in a profile stops the
// adapter at start rather than dropping events at night. See
// config/zigbee.example.json and config/profiles/.
package zigbeeconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"safehome/internal/contract"
	"safehome/internal/zigbee"
	"safehome/internal/zigbeeapp"
)

// Result is what the files describe.
type Result struct {
	App       zigbeeapp.Config
	BrokerURL string
	ClientID  string
	StateDir  string
	LogLevel  slog.Level
}

type file struct {
	Site         string `json:"site"`
	BrokerURL    string `json:"broker_url"`
	ClientID     string `json:"client_id"`
	StateDir     string `json:"state_dir"`
	LogLevel     string `json:"log_level"`
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
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("zigbeeconfig: %w", err)
	}
	r, err := parse(data, filepath.Dir(path))
	if err != nil {
		return Result{}, fmt.Errorf("zigbeeconfig: %s: %w", path, err)
	}
	return r, nil
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if dec.More() {
		return errors.New("invalid JSON: trailing data")
	}
	return nil
}

func parse(data []byte, dir string) (Result, error) {
	var f file
	if err := decodeStrict(data, &f); err != nil {
		return Result{}, err
	}
	if f.StateDir == "" {
		return Result{}, errors.New("state_dir is required (where the adapter keeps its registry)")
	}
	if f.ProfilesDir == "" {
		return Result{}, errors.New("profiles_dir is required (the folder of the device profiles)")
	}
	r := Result{BrokerURL: f.BrokerURL, ClientID: f.ClientID, StateDir: f.StateDir}
	if r.BrokerURL == "" {
		r.BrokerURL = "mqtt://127.0.0.1:1883"
	}
	if r.ClientID == "" {
		r.ClientID = "safehome-zigbee-" + f.Site
	}
	level, err := parseLevel(f.LogLevel)
	if err != nil {
		return Result{}, err
	}
	r.LogLevel = level
	if r.App, err = f.app(dir); err != nil {
		return Result{}, err
	}
	return r, nil
}

// app builds the adapter's settings: the defaults, with what the file overrides.
func (f file) app(dir string) (zigbeeapp.Config, error) {
	profilesDir := f.ProfilesDir
	if !filepath.IsAbs(profilesDir) {
		profilesDir = filepath.Join(dir, profilesDir)
	}
	profiles, err := LoadProfiles(profilesDir)
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
		if err := duration(d.name, d.value, d.dst); err != nil {
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
	if err := decodeStrict(data, &f); err != nil {
		return zigbee.Profile{}, err
	}
	p := zigbee.Profile{
		Name: f.Name, Models: f.Models,
		Capabilities: map[string]zigbee.Rule{}, Commands: map[string]zigbee.ProfileCommand{},
	}
	if err := duration("offline_after", f.OfflineAfter, &p.OfflineAfter); err != nil {
		return zigbee.Profile{}, err
	}
	for c, rf := range f.Capabilities {
		rule := zigbee.Rule{From: rf.From, Map: rf.Map, Max: rf.Max, Step: rf.Step, Momentary: rf.Momentary, CodeFrom: rf.CodeFrom}
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
	if err := (zigbee.Config{Profiles: []zigbee.Profile{p}, ConfirmWait: 1, Attempts: 1, OfflineAfter: 1, PairMax: 1, Step: 1}).Validate(); err != nil {
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

func duration(name, value string, dst *time.Duration) error {
	if value == "" {
		return nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s %q: %w (write for example 3s, 10m or 4h)", name, value, err)
	}
	*dst = d
	return nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("log_level %q: use debug, info, warn or error", s)
}
