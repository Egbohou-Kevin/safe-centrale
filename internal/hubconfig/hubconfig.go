// Package hubconfig reads the hub's configuration file (JSON) and turns it into the
// settings of the application and of the domains. It is strict on purpose: a typo in
// a key is an error, not a default applied in silence, because a misspelt delay on
// an alarm is a security problem.
//
// Only what differs from the documented defaults needs to be written. See
// config/hub.example.json for a complete file.
package hubconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // the hub's image may have no time zone database

	"safehome/internal/contract"
	"safehome/internal/engine"
	"safehome/internal/escalation"
	escwire "safehome/internal/escalation/wire"
	"safehome/internal/health"
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

type zoneFile struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type alarmFile struct {
	ExitDelay       string `json:"exit_delay"`
	EntryDelay      string `json:"entry_delay"`
	SirenCycle      string `json:"siren_cycle"`
	SirenPause      string `json:"siren_pause"`
	SirenCycles     *int   `json:"siren_cycles"`
	LockoutAttempts *int   `json:"lockout_attempts"`
	LockoutDuration string `json:"lockout_duration"`
	PanicSiren      *bool  `json:"panic_siren"`
	AutoArmNotice   string `json:"auto_arm_notice"`
	PostponeStep    string `json:"postpone_step"`
	PostponeMax     *int   `json:"postpone_max"`
	AutoArmLimit    string `json:"auto_arm_limit"`
}

type healthFile struct {
	HubBatteryWarn     *int   `json:"hub_battery_warn"`
	HubBatteryCritical *int   `json:"hub_battery_critical"`
	HubBatteryRearm    *int   `json:"hub_battery_rearm"`
	DeviceBatteryLow   *int   `json:"device_battery_low"`
	DeviceBatteryRearm *int   `json:"device_battery_rearm"`
	SummaryAt          string `json:"summary_at"`
	GSMDegradedAfter   string `json:"gsm_degraded_after"`
	GSMSilentAfter     string `json:"gsm_silent_after"`
	ZigbeeSilentAfter  string `json:"zigbee_silent_after"`
	ZigbeeDownAfter    string `json:"zigbee_down_after"`
	InternetSilent     string `json:"internet_silent_after"`
}

type accessFile struct {
	Iterations *int `json:"iterations"`
}

type contactFile struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	Channels []string `json:"channels"`
}

type escalationFile struct {
	InternetConfirm string              `json:"internet_confirm"`
	GSMDelay        string              `json:"gsm_delay"`
	MaxPerHour      *int                `json:"max_per_hour"`
	Plans           map[string]planFile `json:"plans"`
}

// planFile is the plan of one kind of alert. Written, it replaces the default plan of
// that kind entirely; a kind left out keeps its default (docs/escalation.md).
type planFile struct {
	Waves        []waveFile  `json:"waves"`
	Repeat       *repeatFile `json:"repeat"`
	InternetOnly bool        `json:"internet_only"`
	GSMAfter     string      `json:"gsm_after"`
}

type waveFile struct {
	After  string   `json:"after"`
	To     []string `json:"to"`
	SMSNow bool     `json:"sms_now"`
	Double bool     `json:"double"`
}

type repeatFile struct {
	Every string `json:"every"`
	Times int    `json:"times"`
}

type file struct {
	Site           string              `json:"site"`
	SiteName       string              `json:"site_name"`
	Contacts       []contactFile       `json:"contacts"`
	Escalation     *escalationFile     `json:"escalation"`
	BrokerURL      string              `json:"broker_url"`
	ClientID       string              `json:"client_id"`
	StateDir       string              `json:"state_dir"`
	LogLevel       string              `json:"log_level"`
	Timezone       string              `json:"timezone"`
	HubDeviceID    string              `json:"hub_device_id"`
	SirenIDs       []string            `json:"siren_ids"`
	Zones          map[string]zoneFile `json:"zones"`
	Closing        map[string]string   `json:"closing"`
	Alarm          *alarmFile          `json:"alarm"`
	Health         *healthFile         `json:"health"`
	Access         *accessFile         `json:"access"`
	HeartbeatEvery string              `json:"heartbeat_every"`
	BootWindow     string              `json:"boot_window"`
	Versions       map[string]string   `json:"versions"`
}

// Load reads and checks the file at path, lays over it the zones and sirens the
// installer's tools keep in the state folder (DevicesFile), then the configuration
// set from the cloud (internal/siteconfig). Neither ever stops the hub: one that does
// not fit is left aside, and DevicesErr or SiteErr says why.
func Load(path string) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("hubconfig: %w", err)
	}
	f, err := decode(data)
	if err != nil {
		return Result{}, fmt.Errorf("hubconfig: %s: %w", path, err)
	}
	base := f
	var devErr error
	digest := ""
	if f.StateDir != "" {
		d, s, found, err := readDevices(f.StateDir)
		switch {
		case err != nil:
			devErr = err
		case found:
			withDevices(&f, d)
			digest = s
		}
	}
	r, err := build(f)
	if err != nil && digest != "" {
		devErr = fmt.Errorf("hubconfig: %s left aside: %w", DevicesFile, err)
		f, digest = base, ""
		r, err = build(f)
	}
	if err != nil {
		return Result{}, fmt.Errorf("hubconfig: %s: %w", path, err)
	}
	r.DevicesSum, r.DevicesErr = digest, devErr

	o, ok, err := siteconfig.Read(r.StateDir)
	switch {
	case err != nil:
		r.SiteErr = err
	case ok:
		merged, err := withSite(f, o)
		if err != nil {
			r.SiteVersion, r.SiteErr = o.Version, err
			return r, nil
		}
		merged.SiteVersion = o.Version
		merged.DevicesSum, merged.DevicesErr = digest, devErr
		return merged, nil
	}
	return r, nil
}

// WithSiteConfig returns the configuration at path, with the devices of the state
// folder, and o laid over it: the check the hub makes before accepting a
// configuration from the cloud.
func WithSiteConfig(path string, o contract.ConfigSetData) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("hubconfig: %w", err)
	}
	f, err := decode(data)
	if err != nil {
		return Result{}, err
	}
	if f.StateDir != "" {
		if d, _, found, err := readDevices(f.StateDir); err == nil && found {
			withDevices(&f, d)
		}
	}
	return withSite(f, o)
}

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
		f.Alarm = &alarm
		if o.ExitDelayS != 0 {
			f.Alarm.ExitDelay = fmt.Sprintf("%ds", o.ExitDelayS)
		}
		if o.EntryDelayS != 0 {
			f.Alarm.EntryDelay = fmt.Sprintf("%ds", o.EntryDelayS)
		}
	}
	return build(f)
}

// Parse reads and checks a configuration.
func Parse(data []byte) (Result, error) {
	f, err := decode(data)
	if err != nil {
		return Result{}, err
	}
	return build(f)
}

func decode(data []byte) (file, error) {
	var f file
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return file{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if dec.More() {
		return file{}, errors.New("invalid JSON: trailing data after the configuration")
	}
	return f, nil
}

func build(f file) (Result, error) {

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
	level, err := parseLevel(f.LogLevel)
	if err != nil {
		return Result{}, err
	}
	r.LogLevel = level

	cfg := hub.DefaultConfig(f.Site, f.HubDeviceID, f.SirenIDs...)
	cfg.Versions = f.Versions

	if f.Timezone != "" {
		loc, err := time.LoadLocation(f.Timezone)
		if err != nil {
			return Result{}, fmt.Errorf("timezone %q: %w", f.Timezone, err)
		}
		cfg.Engine.Location = loc
		cfg.Health.Location = loc
	}
	if cfg.Engine.Zones, err = zones(f.Zones); err != nil {
		return Result{}, err
	}
	if cfg.Engine.Closing, err = closing(f.Closing); err != nil {
		return Result{}, err
	}
	if err := applyAlarm(&cfg.Engine, f.Alarm); err != nil {
		return Result{}, err
	}
	if err := applyHealth(&cfg.Health, f.Health); err != nil {
		return Result{}, err
	}
	if err := applyEscalation(&cfg, f); err != nil {
		return Result{}, err
	}
	if f.Access != nil && f.Access.Iterations != nil {
		cfg.Access.Iterations = *f.Access.Iterations
	}
	if err := duration("heartbeat_every", f.HeartbeatEvery, &cfg.HeartbeatEvery); err != nil {
		return Result{}, err
	}
	if err := duration("boot_window", f.BootWindow, &cfg.BootWindow); err != nil {
		return Result{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	r.Hub = cfg
	return r, nil
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

func zones(in map[string]zoneFile) (map[string]engine.Zone, error) {
	out := make(map[string]engine.Zone, len(in))
	for id, z := range in {
		if z.Name == "" {
			return nil, fmt.Errorf("zone of %q has no name", id)
		}
		out[id] = engine.Zone{Name: z.Name, Kind: engine.ZoneKind(z.Kind)} // the kind is checked by the engine
	}
	return out, nil
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// closing reads the closing times. "daily" sets every day, a day name overrides it,
// and the value "off" means no automatic arming that day.
func closing(in map[string]string) (map[time.Weekday]engine.TimeOfDay, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := map[time.Weekday]engine.TimeOfDay{}
	set := func(day time.Weekday, key, value string) error {
		if value == "off" {
			delete(out, day)
			return nil
		}
		t, err := clock(value)
		if err != nil {
			return fmt.Errorf("closing %q: %w", key, err)
		}
		out[day] = t
		return nil
	}
	if v, ok := in["daily"]; ok {
		for _, day := range weekdays {
			if err := set(day, "daily", v); err != nil {
				return nil, err
			}
		}
	}
	for key, value := range in {
		if key == "daily" {
			continue
		}
		day, ok := weekdays[key]
		if !ok {
			return nil, fmt.Errorf("closing: unknown day %q (use mon..sun or daily)", key)
		}
		if err := set(day, key, value); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func clock(s string) (engine.TimeOfDay, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return engine.TimeOfDay{}, fmt.Errorf("%q is not a time of day, write HH:MM", s)
	}
	return engine.TimeOfDay{Hour: t.Hour(), Minute: t.Minute()}, nil
}

func duration(name, value string, dst *time.Duration) error {
	if value == "" {
		return nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s %q: %w (write for example 45s, 2m or 1h30m)", name, value, err)
	}
	*dst = d
	return nil
}

func applyAlarm(c *engine.Config, a *alarmFile) error {
	if a == nil {
		return nil
	}
	for _, d := range []struct {
		name, value string
		dst         *time.Duration
	}{
		{"alarm.exit_delay", a.ExitDelay, &c.ExitDelay},
		{"alarm.entry_delay", a.EntryDelay, &c.EntryDelay},
		{"alarm.siren_cycle", a.SirenCycle, &c.SirenCycle},
		{"alarm.siren_pause", a.SirenPause, &c.SirenPause},
		{"alarm.lockout_duration", a.LockoutDuration, &c.LockoutDuration},
		{"alarm.auto_arm_notice", a.AutoArmNotice, &c.AutoArmNotice},
		{"alarm.postpone_step", a.PostponeStep, &c.PostponeStep},
		{"alarm.auto_arm_limit", a.AutoArmLimit, &c.AutoArmLimit},
	} {
		if err := duration(d.name, d.value, d.dst); err != nil {
			return err
		}
	}
	setInt(&c.SirenCycles, a.SirenCycles)
	setInt(&c.LockoutAttempts, a.LockoutAttempts)
	setInt(&c.PostponeMax, a.PostponeMax)
	if a.PanicSiren != nil {
		c.PanicSiren = *a.PanicSiren
	}
	return nil
}

func applyHealth(c *health.Config, h *healthFile) error {
	if h == nil {
		return nil
	}
	setInt(&c.HubBatteryWarn, h.HubBatteryWarn)
	setInt(&c.HubBatteryCritical, h.HubBatteryCritical)
	setInt(&c.HubBatteryRearm, h.HubBatteryRearm)
	setInt(&c.DeviceBatteryLow, h.DeviceBatteryLow)
	setInt(&c.DeviceBatteryRearm, h.DeviceBatteryRearm)
	if h.SummaryAt != "" {
		t, err := clock(h.SummaryAt)
		if err != nil {
			return fmt.Errorf("health.summary_at: %w", err)
		}
		c.SummaryAt = health.TimeOfDay{Hour: t.Hour, Minute: t.Minute}
	}
	if err := duration("health.gsm_degraded_after", h.GSMDegradedAfter, &c.GSMDegradedAfter); err != nil {
		return err
	}
	for _, d := range []struct {
		name, value string
		dst         *time.Duration
	}{
		{"health.gsm_silent_after", h.GSMSilentAfter, &c.GSMSilentAfter},
		{"health.zigbee_silent_after", h.ZigbeeSilentAfter, &c.ZigbeeSilentAfter},
		{"health.zigbee_down_after", h.ZigbeeDownAfter, &c.ZigbeeDownAfter},
		{"health.internet_silent_after", h.InternetSilent, &c.InternetSilentAfter},
	} {
		if err := duration(d.name, d.value, d.dst); err != nil {
			return err
		}
	}
	return nil
}

func setInt(dst *int, v *int) {
	if v != nil {
		*dst = *v
	}
}

// applyEscalation reads who is told (the contacts) and how. The site name is for
// the texts; the zones' names tell the contacts which door opened.
func applyEscalation(cfg *hub.Config, f file) error {
	e := &cfg.Escalation
	e.SiteName = f.SiteName
	if e.SiteName == "" {
		e.SiteName = f.Site
	}
	e.Location = cfg.Engine.Location
	e.DeviceNames = make(map[string]string, len(cfg.Engine.Zones))
	for id, z := range cfg.Engine.Zones {
		e.DeviceNames[id] = z.Name
	}
	for _, c := range f.Contacts {
		contact := escalation.Contact{ID: c.ID, Name: c.Name, Role: escalation.Role(c.Role)}
		for _, ch := range c.Channels {
			contact.Channels = append(contact.Channels, escalation.Channel(ch))
		}
		e.Contacts = append(e.Contacts, contact)
	}
	if f.Escalation == nil {
		return nil
	}
	if err := duration("escalation.internet_confirm", f.Escalation.InternetConfirm, &e.InternetConfirm); err != nil {
		return err
	}
	if err := duration("escalation.gsm_delay", f.Escalation.GSMDelay, &e.GSMDelay); err != nil {
		return err
	}
	setInt(&e.MaxPerHour, f.Escalation.MaxPerHour)
	return applyPlans(e, f.Escalation.Plans)
}

// applyPlans reads the plans, named as the contract names the alerts. The escalation
// checks them (order of the waves, audiences...) when the configuration is validated.
func applyPlans(e *escalation.Config, plans map[string]planFile) error {
	if len(plans) == 0 {
		return nil
	}
	names := make([]string, 0, len(plans))
	for name := range plans {
		names = append(names, name)
	}
	slices.Sort(names) // the same error first, whatever the map order
	merged := maps.Clone(e.Plans)
	if merged == nil {
		merged = escalation.DefaultPlans()
	}
	for _, name := range names {
		kind, ok := escwire.Kind(name)
		if !ok {
			return fmt.Errorf("escalation.plans: unknown alert kind %q (use %s)", name, strings.Join(escwire.KindNames(), ", "))
		}
		p, err := plan("escalation.plans."+name, plans[name])
		if err != nil {
			return err
		}
		if err := p.Validate(); err != nil {
			return fmt.Errorf("escalation.plans.%s: %w", name, err)
		}
		merged[kind] = p
	}
	e.Plans = merged
	return nil
}

func plan(path string, f planFile) (escalation.Plan, error) {
	p := escalation.Plan{InternetOnly: f.InternetOnly}
	for i, w := range f.Waves {
		wave := escalation.Wave{SMSNow: w.SMSNow, Double: w.Double}
		if err := duration(fmt.Sprintf("%s.waves[%d].after", path, i+1), w.After, &wave.After); err != nil {
			return escalation.Plan{}, err
		}
		for _, a := range w.To {
			wave.To = append(wave.To, escalation.Audience(a)) // the escalation checks the names
		}
		p.Waves = append(p.Waves, wave)
	}
	if f.Repeat != nil {
		p.Repeat.Times = f.Repeat.Times
		if err := duration(path+".repeat.every", f.Repeat.Every, &p.Repeat.Every); err != nil {
			return escalation.Plan{}, err
		}
	}
	if err := duration(path+".gsm_after", f.GSMAfter, &p.GSMAfter); err != nil {
		return escalation.Plan{}, err
	}
	return p, nil
}
