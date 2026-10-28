package contract

import (
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"slices"
)

// The site's configuration set from the cloud (see docs/contract.md): what the owner
// changes in the app. It carries the whole of it each time, numbered by version; a
// part left out keeps the value of the site's own files. Zones, sirens and devices
// are not in it: they are set on site only.

const (
	TypeConfigSet     = "config.set"
	TypeConfigApplied = "config.applied"
)

// ConfigSetTopic is where the cloud publishes the configuration; ConfigAppliedTopic
// where the site answers.
func ConfigSetTopic(site string) string     { return Topic(site, "config", "set") }
func ConfigAppliedTopic(site string) string { return Topic(site, "config", "applied") }

// The bounds of what the cloud may set: a cloud that is wrong, or hostile, cannot
// make the alarm useless (a ten-minute entry delay).
const (
	MinDelayS      = 10
	MaxExitDelayS  = 300
	MaxEntryDelayS = 180
	MaxContacts    = 5 // as the escalation (escalation.MaxContacts)
)

// MaxQuotaMB bounds the monthly video allowance far above any client's (8 TB), so
// that it never overflows once in bytes. The video refuses more as well.
const MaxQuotaMB = 1 << 23

// MaxConfigVersion is the highest version the cloud can number: it counts the seconds
// from 2026-01-01 in a 32-bit int (enough until 2094). A higher one is a mistake or a
// forgery, and once kept it would make every later configuration "older": refused.
// A forged one below it, far ahead of the clock, is still kept: open, Kevin's choice.
const MaxConfigVersion = math.MaxInt32

// ConfigContact is a person to alert. Phone, in international form (+22890000001),
// is for the GSM channels; without it, the person gets only the app and WhatsApp.
type ConfigContact struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Role     string   `json:"role"` // owner, deputy, guard, manager, other
	Channels []string `json:"channels"`
	Phone    string   `json:"phone,omitempty"`
}

// ConfigVideo is the client's setting on mobile data: allowance per month and
// longest session, 0 for none. Ignored on a fixed line.
type ConfigVideo struct {
	QuotaMB *int `json:"quota_mb,omitempty"`
	MaxS    *int `json:"max_s,omitempty"`
}

// ConfigSetData: config.set, from the cloud on config/set. It holds phone numbers:
// never logged. A list not sent (nil) leaves the program's file in charge; an empty
// list sent ("contacts": [], "closing": {}) is a list, kept as such when saved
// (omitzero, not omitempty, which would drop it and let the file's lists come back).
type ConfigSetData struct {
	Version     int               `json:"version"`
	SiteName    string            `json:"site_name,omitempty"`
	Contacts    []ConfigContact   `json:"contacts,omitzero"`
	Closing     map[string]string `json:"closing,omitzero"` // "daily", "mon".."sun" -> "20:00" or "off"
	ExitDelayS  int               `json:"exit_delay_s,omitempty"`
	EntryDelayS int               `json:"entry_delay_s,omitempty"`
	Video       *ConfigVideo      `json:"video,omitempty"`
}

// Statuses of config.applied.
const (
	ConfigApplied  = "applied"
	ConfigRejected = "rejected"
)

// ConfigAppliedData: config.applied, from the site on config/applied.
type ConfigAppliedData struct {
	Version int    `json:"version"`
	Status  string `json:"status"`           // applied, rejected
	Reason  string `json:"reason,omitempty"` // for rejected
}

var (
	contactID   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	phoneNumber = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	channels    = []string{ChannelApp, ChannelWhatsApp, ChannelSMS, ChannelCall}
	configRoles = []string{"owner", "deputy", "guard", "manager", "other"}
)

// ValidPhone reports whether s is a phone number in international form.
func ValidPhone(s string) bool { return phoneNumber.MatchString(s) }

// String hides the phone numbers.
func (d ConfigSetData) String() string {
	return fmt.Sprintf("{version:%d site_name:%q contacts:%d closing:%v exit_delay_s:%d entry_delay_s:%d video:%v}",
		d.Version, d.SiteName, len(d.Contacts), d.Closing, d.ExitDelayS, d.EntryDelayS, d.Video != nil)
}

// GoString hides them for %#v.
func (d ConfigSetData) GoString() string { return d.String() }

// LogValue hides them in slog logs, whatever the handler (a JSON one would print the
// fields otherwise).
func (d ConfigSetData) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("version", d.Version), slog.String("site_name", d.SiteName), slog.Int("contacts", len(d.Contacts)),
		slog.Any("closing", d.Closing), slog.Int("exit_delay_s", d.ExitDelayS), slog.Int("entry_delay_s", d.EntryDelayS),
		slog.Bool("video", d.Video != nil),
	)
}

// Validate checks the shape. The site checks the rest (roles, closing times) against
// its own rules before applying anything. No phone number is part of an error.
func (d ConfigSetData) Validate() error {
	switch {
	case d.Version < 1:
		return fmt.Errorf("%w: version must be at least 1", ErrInvalid)
	case d.Version > MaxConfigVersion:
		return fmt.Errorf("%w: version beyond %d, which the cloud never numbers", ErrInvalid, MaxConfigVersion)
	case d.Contacts != nil && len(d.Contacts) == 0:
		// A list sent replaces the hub's: an empty one would leave every alarm unheard.
		return fmt.Errorf("%w: a site keeps at least one contact", ErrInvalid)
	case len(d.Contacts) > MaxContacts:
		return fmt.Errorf("%w: %d contacts at most", ErrInvalid, MaxContacts)
	case d.ExitDelayS != 0 && (d.ExitDelayS < MinDelayS || d.ExitDelayS > MaxExitDelayS):
		return fmt.Errorf("%w: exit_delay_s must be from %d to %d", ErrInvalid, MinDelayS, MaxExitDelayS)
	case d.EntryDelayS != 0 && (d.EntryDelayS < MinDelayS || d.EntryDelayS > MaxEntryDelayS):
		return fmt.Errorf("%w: entry_delay_s must be from %d to %d", ErrInvalid, MinDelayS, MaxEntryDelayS)
	}
	seen := map[string]bool{}
	for _, c := range d.Contacts {
		if seen[c.ID] {
			return fmt.Errorf("%w: contact id %q invalid or repeated", ErrInvalid, c.ID)
		}
		if err := c.validate(); err != nil {
			return err
		}
		seen[c.ID] = true
	}
	if v := d.Video; v != nil {
		if (v.QuotaMB != nil && (*v.QuotaMB < 0 || *v.QuotaMB > MaxQuotaMB)) || (v.MaxS != nil && (*v.MaxS < 0 || *v.MaxS > MaxVideoLength)) {
			return fmt.Errorf("%w: video quota_mb from 0 to %d, max_s from 0 to %d", ErrInvalid, MaxQuotaMB, MaxVideoLength)
		}
	}
	return nil
}

// validate checks one contact. No phone number is part of an error.
func (c ConfigContact) validate() error {
	switch {
	case !contactID.MatchString(c.ID):
		return fmt.Errorf("%w: contact id %q invalid or repeated", ErrInvalid, c.ID)
	case c.Name == "" || !slices.Contains(configRoles, c.Role):
		return fmt.Errorf("%w: contact %s needs a name and a known role", ErrInvalid, c.ID)
	case len(c.Channels) == 0:
		return fmt.Errorf("%w: contact %s has no channel", ErrInvalid, c.ID)
	case c.Phone != "" && !ValidPhone(c.Phone):
		return fmt.Errorf("%w: contact %s: phone not in international form (+228...)", ErrInvalid, c.ID)
	}
	for _, ch := range c.Channels {
		if !slices.Contains(channels, ch) {
			return fmt.Errorf("%w: contact %s: unknown channel %q", ErrInvalid, c.ID, ch)
		}
		if (ch == ChannelSMS || ch == ChannelCall) && c.Phone == "" {
			return fmt.Errorf("%w: contact %s: %s needs a phone number", ErrInvalid, c.ID, ch)
		}
	}
	return nil
}

// Validate checks the answer.
func (d ConfigAppliedData) Validate() error {
	switch {
	case d.Version < 0:
		return fmt.Errorf("%w: negative version", ErrInvalid)
	case d.Status != ConfigApplied && d.Status != ConfigRejected:
		return fmt.Errorf("%w: status %q", ErrInvalid, d.Status)
	case d.Status == ConfigRejected && d.Reason == "":
		return fmt.Errorf("%w: a rejection says why", ErrInvalid)
	}
	return nil
}
