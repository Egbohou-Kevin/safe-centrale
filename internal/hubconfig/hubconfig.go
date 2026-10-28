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
	"time"
	_ "time/tzdata" // the hub's image may have no time zone database

	"safehome/internal/configfile"
	"safehome/internal/contract"
	"safehome/internal/hub"
	"safehome/internal/siteconfig"
)

// Result is what the file describes.
type Result struct {
	configfile.Program
	Hub hub.Config

	// SiteVersion is the version of the configuration set from the cloud found in
	// the state folder (0: none). SiteErr says why it was not applied: the hub then
	// runs on its own file alone, and says so to the cloud.
	SiteVersion int
	SiteErr     error

	// DevicesSum identifies the devices file the hub loaded (empty: none, the zones
	// and sirens of the file at path). DevicesErr says why it was left aside.
	DevicesSum string
	DevicesErr error

	// RoomsLeftAside names the zones of the devices file whose room the hub would
	// refuse: each zone is watched without its room, never left aside for it.
	RoomsLeftAside []RoomLeftAside
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
	f, digest, rooms, devErr := withStateDevices(f)
	r, err := build(f)
	if err != nil {
		return Result{}, fmt.Errorf("hubconfig: %s: %w", path, err)
	}
	r.DevicesSum, r.DevicesErr, r.RoomsLeftAside = digest, devErr, rooms
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
	f, _, _, _ = withStateDevices(f)
	return withSite(f, o)
}

// withStateDevices lays over f the devices file of its state folder. A file that
// cannot be read, or that the hub would refuse, is left aside and devErr says why;
// digest identifies the file laid over f. A room the hub would refuse costs only
// itself: it is cleared, and rooms names its zone.
func withStateDevices(f file) (out file, digest string, rooms []RoomLeftAside, devErr error) {
	if f.StateDir == "" {
		return f, "", nil, nil
	}
	d, digest, found, err := readDevices(f.StateDir)
	if err != nil || !found {
		return f, "", nil, err
	}
	rooms = clearBadRooms(d.Zones)
	out = f // withDevices replaces the zones map, so f keeps its own
	withDevices(&out, d)
	if _, err := build(out); err != nil {
		return f, "", nil, fmt.Errorf("hubconfig: %s left aside: %w", DevicesFile, err)
	}
	return out, digest, rooms, nil
}

// withStoredSite lays over r, built from f, the configuration set from the cloud that
// the state folder keeps. One that does not fit leaves r as it is, with the reason.
func withStoredSite(r Result, f file) Result {
	o, found, err := siteconfig.Read(r.StateDir)
	if err != nil {
		r.SiteVersion, r.SiteErr = o.Version, err // 0 for a file that cannot be read
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
	merged.DevicesSum, merged.DevicesErr, merged.RoomsLeftAside = r.DevicesSum, r.DevicesErr, r.RoomsLeftAside
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
	p, err := f.Build("safehome-hub-", "where the hub keeps its state and its codes")
	if err != nil {
		return Result{}, err
	}
	r := Result{Program: p}
	if r.Hub, err = hubConfig(f); err != nil {
		return Result{}, err
	}
	return r, nil
}

// errNoSirenList refuses a file without siren_ids. Left out, it is a forgotten line
// rather than a choice: a site without a siren says so, as one without a GSM line does.
var errNoSirenList = errors.New(`siren_ids is required: give "siren_ids": [] for a site without a siren (its alarms then only send messages)`)

// hubConfig lays the file over the defaults of the hub and of its domains.
func hubConfig(f file) (hub.Config, error) {
	cfg := hub.DefaultConfig(f.Site, f.HubDeviceID, f.SirenIDs...)
	cfg.Versions = f.Versions
	if f.Timezone != "" {
		// "Local" would be the hub's own setting, unknown elsewhere: the zone goes in
		// the alerts (alert.request), where it must be an IANA name.
		if f.Timezone == "Local" {
			return hub.Config{}, fmt.Errorf("timezone %q: give the site's IANA name, such as Africa/Lome", f.Timezone)
		}
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
	if err := sirenNames(f.SirenIDs, f.Zones, f.SirenNames); err != nil {
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
