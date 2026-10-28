package hubconfig

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"safehome/internal/configfile"
	"safehome/internal/engine"
	"safehome/internal/escalation"
	escwire "safehome/internal/escalation/wire"
	"safehome/internal/health"
	"safehome/internal/hub"
	"safehome/internal/install"
)

// zones reads the zones. A name follows the installer's rule (install.ValidName),
// even in a file edited by hand: it goes into SMS, voice messages and the alarm
// events. A room, when there is one, follows the same rule (install.ValidRoom): it
// goes to the cloud and the app in the device list.
func zones(in map[string]zoneFile) (map[string]engine.Zone, error) {
	out := make(map[string]engine.Zone, len(in))
	for id, z := range in {
		if err := install.ValidName(z.Name); err != nil {
			return nil, fmt.Errorf("zone of %q: %w", id, err)
		}
		if err := install.ValidRoom(z.Room); err != nil {
			return nil, fmt.Errorf("zone of %q: %w", id, err)
		}
		out[id] = engine.Zone{Name: z.Name, Kind: engine.ZoneKind(z.Kind), Room: z.Room} // the kind is checked by the engine
	}
	return out, nil
}

// sirenNames checks the sirens' list, which must be there (errNoSirenList), and their
// names: each names a siren of the list without a zone
// (a device with a zone has its zone's name), by the installer's rule
// (install.ValidSirenName), since it goes into the same texts as a zone's name.
func sirenNames(ids []string, zones map[string]zoneFile, names map[string]string) error {
	if ids == nil {
		return errNoSirenList
	}
	for _, id := range slices.Sorted(maps.Keys(names)) { // the same error first, whatever the map order
		if !slices.Contains(ids, id) {
			return fmt.Errorf("siren_names: %q is not in siren_ids", id)
		}
		if _, zoned := zones[id]; zoned {
			return fmt.Errorf("siren_names: %q has a zone, the alerts call it by its zone's name", id)
		}
		if err := install.ValidSirenName(names[id]); err != nil {
			return fmt.Errorf("siren_names of %q: %w", id, err)
		}
	}
	return nil
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

// durationField is a duration of the file and the setting it goes to.
type durationField struct {
	name, value string
	dst         *time.Duration
}

func durations(fields []durationField) error {
	for _, d := range fields {
		if err := configfile.Duration(d.name, d.value, d.dst); err != nil {
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

func applyAlarm(c *engine.Config, a *alarmFile) error {
	if a == nil {
		return nil
	}
	err := durations([]durationField{
		{"alarm.exit_delay", a.ExitDelay, &c.ExitDelay},
		{"alarm.entry_delay", a.EntryDelay, &c.EntryDelay},
		{"alarm.siren_cycle", a.SirenCycle, &c.SirenCycle},
		{"alarm.siren_pause", a.SirenPause, &c.SirenPause},
		{"alarm.lockout_duration", a.LockoutDuration, &c.LockoutDuration},
		{"alarm.auto_arm_notice", a.AutoArmNotice, &c.AutoArmNotice},
		{"alarm.postpone_step", a.PostponeStep, &c.PostponeStep},
		{"alarm.auto_arm_limit", a.AutoArmLimit, &c.AutoArmLimit},
	})
	if err != nil {
		return err
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
	return durations([]durationField{
		{"health.gsm_degraded_after", h.GSMDegradedAfter, &c.GSMDegradedAfter},
		{"health.gsm_silent_after", h.GSMSilentAfter, &c.GSMSilentAfter},
		{"health.zigbee_silent_after", h.ZigbeeSilentAfter, &c.ZigbeeSilentAfter},
		{"health.zigbee_down_after", h.ZigbeeDownAfter, &c.ZigbeeDownAfter},
		{"health.internet_silent_after", h.InternetSilent, &c.InternetSilentAfter},
	})
}

// applyEscalation reads who is told (the contacts) and how. The site name is for
// the texts; the zones' names tell the contacts which door opened, and the sirens'
// names which siren stays silent (install.Site.SirenName: a device with a zone keeps
// its zone's name).
func applyEscalation(cfg *hub.Config, f file) error {
	e := &cfg.Escalation
	// No SMS line, by choice: the gsm link says so, and no SMS nor call is asked for.
	cfg.Health.WithoutGSM, e.WithoutGSM = f.WithoutGSM, f.WithoutGSM
	e.SiteName = f.SiteName
	if e.SiteName == "" {
		e.SiteName = f.Site
	}
	e.Location = cfg.Engine.Location
	e.DeviceNames = make(map[string]string, len(cfg.Engine.Zones)+len(f.SirenIDs))
	e.ZoneNames = make(map[string]string, len(cfg.Engine.Zones))
	for id, z := range cfg.Engine.Zones {
		e.DeviceNames[id] = z.Name
		e.ZoneNames[id] = z.Name
	}
	sirens := siteOf(f.Zones, f.SirenIDs, f.SirenNames)
	for _, id := range f.SirenIDs {
		e.DeviceNames[id] = sirens.SirenName(id)
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
	err := durations([]durationField{
		{"escalation.internet_confirm", f.Escalation.InternetConfirm, &e.InternetConfirm},
		{"escalation.gsm_delay", f.Escalation.GSMDelay, &e.GSMDelay},
	})
	if err != nil {
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
	merged := maps.Clone(e.Plans)
	if merged == nil {
		merged = escalation.DefaultPlans()
	}
	for _, name := range slices.Sorted(maps.Keys(plans)) { // the same error first, whatever the map order
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
		if err := configfile.Duration(fmt.Sprintf("%s.waves[%d].after", path, i+1), w.After, &wave.After); err != nil {
			return escalation.Plan{}, err
		}
		for _, a := range w.To {
			wave.To = append(wave.To, escalation.Audience(a)) // the escalation checks the names
		}
		p.Waves = append(p.Waves, wave)
	}
	if f.Repeat != nil {
		p.Repeat.Times = f.Repeat.Times
		if err := configfile.Duration(path+".repeat.every", f.Repeat.Every, &p.Repeat.Every); err != nil {
			return escalation.Plan{}, err
		}
	}
	if err := configfile.Duration(path+".gsm_after", f.GSMAfter, &p.GSMAfter); err != nil {
		return escalation.Plan{}, err
	}
	return p, nil
}
