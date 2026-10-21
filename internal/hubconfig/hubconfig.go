// Package hubconfig reads the hub's configuration file (JSON) and turns it into the
// settings of the application and of the domains. It is strict on purpose: a typo in
// a key is an error, not a default applied in silence, because a misspelt delay on
// an alarm is a security problem.
//
// Only what differs from the documented defaults needs to be written. See
// config/hub.example.json for a complete file.
package hubconfig

import (
	"errors"
	"fmt"
	"log/slog"
	"time"
	_ "time/tzdata" // the hub's image may have no time zone database

	"safehome/internal/configfile"
	"safehome/internal/contract"
	"safehome/internal/hub"
	"safehome/internal/siteconfig"
)

// Result is what the file describes.
type Result struct {
	Hub       hub.Config
	BrokerURL string
	ClientID  string
	StateDir  string
	LogLevel  slog.Level

	// SiteVersion is the version of the configuration set from the cloud found in
	// the state folder (0: none). SiteErr says why it was not applied: the hub then
	// runs on its own file alone, and says so to the cloud.
	SiteVersion int
	SiteErr     error

	// DevicesSum identifies the devices file the hub loaded (empty: none, the zones
	// and sirens of the file at path). DevicesErr says why it was left aside.
	DevicesSum string
	DevicesErr error
}

// Load reads and checks the file at path, lays over it the zones and sirens the
// installer's tools keep in the state folder (DevicesFile), then the configuration
// set from the cloud (internal/siteconfig). Neither ever stops the hub: one that does
// not fit is left aside, and DevicesErr or SiteErr says why.
func Load(path string) (Result, error) {
	f, err := readConfig(path)
	if err != nil {
		return Result{}, err
	}
	f, digest, devErr := withStateDevices(f)
	r, err := build(f)
	if err != nil {
		return Result{}, fmt.Errorf("hubconfig: %s: %w", path, err)
	}
	r.DevicesSum, r.DevicesErr = digest, devErr
	return withStoredSite(r, f), nil
}

// WithSiteConfig returns the configuration at path, with the devices of the state
// folder as Load lays them, and o laid over it: the check the hub makes before
// accepting a configuration from the cloud.
func WithSiteConfig(path string, o contract.ConfigSetData) (Result, error) {
	f, err := readConfig(path)
	if err != nil {
		return Result{}, err
	}
	f, _, _ = withStateDevices(f)
	return withSite(f, o)
}

// Parse reads and checks a configuration.
func Parse(data []byte) (Result, error) {
	var f file
	if err := configfile.DecodeBytes(data, &f); err != nil {
		return Result{}, err
	}
	return build(f)
}

// withStateDevices lays over f the devices file of its state folder. A file that
// cannot be read, or that the hub would refuse, is left aside and devErr says why;
// digest identifies the file laid over f.
func withStateDevices(f file) (out file, digest string, devErr error) {
	if f.StateDir == "" {
		return f, "", nil
	}
	d, digest, found, err := readDevices(f.StateDir)
	if err != nil || !found {
		return f, "", err
	}
	out = f // withDevices replaces the zones map, so f keeps its own
	withDevices(&out, d)
	if _, err := build(out); err != nil {
		return f, "", fmt.Errorf("hubconfig: %s left aside: %w", DevicesFile, err)
	}
	return out, digest, nil
}

// withStoredSite lays over r, built from f, the configuration set from the cloud that
// the state folder keeps. One that does not fit leaves r as it is, with the reason.
func withStoredSite(r Result, f file) Result {
	o, found, err := siteconfig.Read(r.StateDir)
	if err != nil {
		r.SiteErr = err
		return r
	}
	if !found {
		return r
	}
	merged, err := withSite(f, o)
	if err != nil {
		r.SiteVersion, r.SiteErr = o.Version, err
		return r
	}
	merged.SiteVersion = o.Version
	merged.DevicesSum, merged.DevicesErr = r.DevicesSum, r.DevicesErr
	return merged
}

// withSite lays the configuration set from the cloud over f, and builds the result.
func withSite(f file, o contract.ConfigSetData) (Result, error) {
	if err := o.Validate(); err != nil {
		return Result{}, err
	}
	if o.SiteName != "" {
		f.SiteName = o.SiteName
	}
	if o.Contacts != nil {
		f.Contacts = nil
		for _, c := range o.Contacts {
			f.Contacts = append(f.Contacts, contactFile{ID: c.ID, Name: c.Name, Role: c.Role, Channels: c.Channels})
		}
	}
	if o.Closing != nil {
		f.Closing = o.Closing
	}
	if o.ExitDelayS != 0 || o.EntryDelayS != 0 {
		alarm := alarmFile{} // a copy: the file's own stays as read
		if f.Alarm != nil {
			alarm = *f.Alarm
		}
		if o.ExitDelayS != 0 {
			alarm.ExitDelay = fmt.Sprintf("%ds", o.ExitDelayS)
		}
		if o.EntryDelayS != 0 {
			alarm.EntryDelay = fmt.Sprintf("%ds", o.EntryDelayS)
		}
		f.Alarm = &alarm
	}
	return build(f)
}

// build turns a file into the settings, and checks them all.
func build(f file) (Result, error) {
	r, err := programSettings(f)
	if err != nil {
		return Result{}, err
	}
	if r.Hub, err = hubConfig(f); err != nil {
		return Result{}, err
	}
	return r, nil
}

// programSettings reads what the program needs around the hub: the broker, the
// state folder, the log level.
func programSettings(f file) (Result, error) {
	if f.StateDir == "" {
		return Result{}, errors.New("state_dir is required (where the hub keeps its state and its codes)")
	}
	r := Result{BrokerURL: f.BrokerURL, ClientID: f.ClientID, StateDir: f.StateDir}
	if r.BrokerURL == "" {
		r.BrokerURL = "mqtt://127.0.0.1:1883"
	}
	if r.ClientID == "" {
		r.ClientID = "safehome-hub-" + f.Site
	}
	var err error
	if r.LogLevel, err = configfile.Level(f.LogLevel); err != nil {
		return Result{}, err
	}
	return r, nil
}

// hubConfig lays the file over the defaults of the hub and of its domains.
func hubConfig(f file) (hub.Config, error) {
	cfg := hub.DefaultConfig(f.Site, f.HubDeviceID, f.SirenIDs...)
	cfg.Versions = f.Versions
	if f.Timezone != "" {
		loc, err := time.LoadLocation(f.Timezone)
		if err != nil {
			return hub.Config{}, fmt.Errorf("timezone %q: %w", f.Timezone, err)
		}
		cfg.Engine.Location = loc
		cfg.Health.Location = loc
	}
	var err error
	if cfg.Engine.Zones, err = zones(f.Zones); err != nil {
		return hub.Config{}, err
	}
	if cfg.Engine.Closing, err = closing(f.Closing); err != nil {
		return hub.Config{}, err
	}
	if err := applyAlarm(&cfg.Engine, f.Alarm); err != nil {
		return hub.Config{}, err
	}
	if err := applyHealth(&cfg.Health, f.Health); err != nil {
		return hub.Config{}, err
	}
	if err := applyEscalation(&cfg, f); err != nil {
		return hub.Config{}, err
	}
	if f.Access != nil {
		setInt(&cfg.Access.Iterations, f.Access.Iterations)
	}
	err = durations([]durationField{
		{"heartbeat_every", f.HeartbeatEvery, &cfg.HeartbeatEvery},
		{"boot_window", f.BootWindow, &cfg.BootWindow},
	})
	if err != nil {
		return hub.Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return hub.Config{}, err
	}
	return cfg, nil
}
