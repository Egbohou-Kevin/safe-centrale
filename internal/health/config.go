package health

import (
	"fmt"
	"time"
)

// TimeOfDay is a wall-clock time in the site's time zone.
type TimeOfDay struct{ Hour, Minute int }

func (t TimeOfDay) valid() bool {
	return t.Hour >= 0 && t.Hour < 24 && t.Minute >= 0 && t.Minute < 60
}

// Config holds the supervision settings, all adjustable per site.
type Config struct {
	// HubDeviceID is the battery module of the hub: its mains and battery readings
	// are the hub's, not those of a sensor.
	HubDeviceID string

	// Hub battery thresholds, in percent. Each one raises one alert per outage and is
	// armed again once the battery climbs above HubBatteryRearm.
	HubBatteryWarn     int
	HubBatteryCritical int
	HubBatteryRearm    int

	// Sensor batteries: low at or below DeviceBatteryLow, fine again above
	// DeviceBatteryRearm. The gap stops a reading that hovers around the threshold
	// from flapping.
	DeviceBatteryLow   int
	DeviceBatteryRearm int

	// SummaryAt is when the daily summary of the faults goes out, in Location (nil
	// means UTC, which is Lome).
	SummaryAt TimeOfDay
	Location  *time.Location

	// GSMDegradedAfter is how long the GSM backup must stay unusable before the
	// owner is told: a line that drops for a minute is not worth a message.
	GSMDegradedAfter time.Duration
	// GSMSilentAfter is how long the GSM gateway may stay silent before its last
	// status stops counting: it repeats its status every minute, so a few missed
	// ones mean it stopped or hangs.
	GSMSilentAfter time.Duration

	// The same for the Zigbee adapter. A blind alarm is worse than a missing SMS
	// backup: the owner is told sooner.
	ZigbeeSilentAfter time.Duration
	ZigbeeDownAfter   time.Duration

	// InternetSilentAfter is how long the link agent may stay silent before the
	// internet counts as down. Nobody is told: the escalation just stops waiting for
	// the internet.
	InternetSilentAfter time.Duration
}

// DefaultConfig returns the defaults decided in docs/escalation.md. HubDeviceID has
// no default: it comes from the site.
func DefaultConfig() Config {
	return Config{
		HubBatteryWarn:     40,
		HubBatteryCritical: 10,
		HubBatteryRearm:    60,
		DeviceBatteryLow:   15,
		DeviceBatteryRearm: 30,
		SummaryAt:          TimeOfDay{Hour: 9},
		GSMDegradedAfter:   10 * time.Minute,
		GSMSilentAfter:     3 * time.Minute,
		ZigbeeSilentAfter:  3 * time.Minute,
		ZigbeeDownAfter:    3 * time.Minute,

		InternetSilentAfter: 3 * time.Minute,
	}
}

// Validate checks that the configuration is usable.
func (c Config) Validate() error {
	if c.HubDeviceID == "" {
		return fmt.Errorf("health: HubDeviceID is required")
	}
	if !(0 < c.HubBatteryCritical && c.HubBatteryCritical < c.HubBatteryWarn &&
		c.HubBatteryWarn < c.HubBatteryRearm && c.HubBatteryRearm <= 100) {
		return fmt.Errorf("health: hub battery thresholds must satisfy 0 < critical (%d) < warn (%d) < rearm (%d) <= 100",
			c.HubBatteryCritical, c.HubBatteryWarn, c.HubBatteryRearm)
	}
	if !(0 < c.DeviceBatteryLow && c.DeviceBatteryLow < c.DeviceBatteryRearm && c.DeviceBatteryRearm <= 100) {
		return fmt.Errorf("health: device battery thresholds must satisfy 0 < low (%d) < rearm (%d) <= 100",
			c.DeviceBatteryLow, c.DeviceBatteryRearm)
	}
	if c.GSMDegradedAfter <= 0 || c.GSMSilentAfter <= 0 || c.ZigbeeSilentAfter <= 0 || c.ZigbeeDownAfter <= 0 || c.InternetSilentAfter <= 0 {
		return fmt.Errorf("health: the GSM, Zigbee and internet delays must be positive")
	}
	if !c.SummaryAt.valid() {
		return fmt.Errorf("health: invalid summary time %v", c.SummaryAt)
	}
	return nil
}

// nextSummary returns the first summary time strictly after from.
func (c Config) nextSummary(from time.Time) time.Time {
	loc := c.Location
	if loc == nil {
		loc = time.UTC
	}
	local := from.In(loc)
	for i := 0; i <= 1; i++ {
		day := local.AddDate(0, 0, i)
		t := time.Date(day.Year(), day.Month(), day.Day(), c.SummaryAt.Hour, c.SummaryAt.Minute, 0, 0, loc)
		if t.After(from) {
			return t.UTC()
		}
	}
	return time.Time{} // unreachable: tomorrow's time is always later
}
