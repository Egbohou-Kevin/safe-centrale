package engine

import (
	"fmt"
	"time"
)

// TimeOfDay is a wall-clock time in the site's time zone.
type TimeOfDay struct{ Hour, Minute int }

func (t TimeOfDay) valid() bool {
	return t.Hour >= 0 && t.Hour < 24 && t.Minute >= 0 && t.Minute < 60
}

// Config holds the engine settings, all adjustable per site.
type Config struct {
	ExitDelay       time.Duration // exit delay
	EntryDelay      time.Duration // entry delay
	SirenCycle      time.Duration // length of one siren cycle
	SirenPause      time.Duration // pause between two cycles
	SirenCycles     int           // maximum number of cycles
	LockoutAttempts int           // wrong codes before lockout
	LockoutDuration time.Duration // how long disarming stays locked
	PanicSiren      bool          // the panic button sounds the siren

	// Automatic arming. Closing gives the closing time of each weekday; a weekday
	// that is missing is not armed automatically, and an empty map turns the
	// feature off. Times are read in Location (nil means UTC, which is Lome).
	Closing  map[time.Weekday]TimeOfDay
	Location *time.Location

	AutoArmNotice time.Duration // how long before closing the system announces it
	PostponeStep  time.Duration // what one postponement adds
	PostponeMax   int           // postponements allowed per closing; 0 forbids them
	// AutoArmLimit is how far past the planned closing time the arming can be
	// pushed back. Postponing never goes beyond it.
	AutoArmLimit time.Duration

	// Zones gives the zone of each device, by device_id. A device missing from
	// this table is ignored, except for its tamper sensor: a keypad or a siren that
	// is opened must still be reported.
	Zones map[string]Zone
}

// DefaultConfig returns the defaults decided in docs/engine.md.
func DefaultConfig() Config {
	return Config{
		ExitDelay:       2 * time.Minute,
		EntryDelay:      45 * time.Second,
		SirenCycle:      5 * time.Minute,
		SirenPause:      2 * time.Minute,
		SirenCycles:     3,
		LockoutAttempts: 3,
		LockoutDuration: 10 * time.Minute,
		PanicSiren:      false,
		AutoArmNotice:   10 * time.Minute,
		PostponeStep:    30 * time.Minute,
		PostponeMax:     3,
		AutoArmLimit:    2 * time.Hour,
		Zones:           map[string]Zone{},
	}
}

// Validate checks that the configuration is usable.
func (c Config) Validate() error {
	positive := map[string]time.Duration{
		"ExitDelay": c.ExitDelay, "EntryDelay": c.EntryDelay, "SirenCycle": c.SirenCycle,
		"SirenPause": c.SirenPause, "LockoutDuration": c.LockoutDuration,
	}
	for name, d := range positive {
		if d <= 0 {
			return fmt.Errorf("engine: %s must be positive, got %v", name, d)
		}
	}
	if c.SirenCycles < 1 {
		return fmt.Errorf("engine: SirenCycles must be at least 1, got %d", c.SirenCycles)
	}
	if c.LockoutAttempts < 1 {
		return fmt.Errorf("engine: LockoutAttempts must be at least 1, got %d", c.LockoutAttempts)
	}
	if len(c.Closing) > 0 {
		for name, d := range map[string]time.Duration{"AutoArmNotice": c.AutoArmNotice, "PostponeStep": c.PostponeStep} {
			if d <= 0 {
				return fmt.Errorf("engine: %s must be positive, got %v", name, d)
			}
		}
		if c.PostponeMax < 0 {
			return fmt.Errorf("engine: PostponeMax must not be negative, got %d", c.PostponeMax)
		}
		if c.AutoArmLimit < 0 {
			return fmt.Errorf("engine: AutoArmLimit must not be negative, got %v", c.AutoArmLimit)
		}
		for day, tod := range c.Closing {
			if day < time.Sunday || day > time.Saturday || !tod.valid() {
				return fmt.Errorf("engine: invalid closing time %v on weekday %d", tod, int(day))
			}
		}
	}
	for id, z := range c.Zones {
		if !z.Kind.valid() {
			return fmt.Errorf("engine: zone %q of device %q: unknown kind %q", z.Name, id, z.Kind)
		}
	}
	return nil
}

// nextClosing returns the first closing time strictly after from, looking at most
// a week ahead. ok is false if no closing time is configured.
func (c Config) nextClosing(from time.Time) (at time.Time, ok bool) {
	loc := c.Location
	if loc == nil {
		loc = time.UTC
	}
	local := from.In(loc)
	for i := 0; i <= 7; i++ {
		day := local.AddDate(0, 0, i)
		tod, set := c.Closing[day.Weekday()]
		if !set {
			continue
		}
		t := time.Date(day.Year(), day.Month(), day.Day(), tod.Hour, tod.Minute, 0, 0, loc)
		if t.After(from) {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
