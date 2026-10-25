package escalation

import (
	"fmt"
	"slices"
	"time"
)

// Contact is a person to tell. The phone number is not here: the channels know it.
type Contact struct {
	ID       string
	Name     string
	Role     Role
	Channels []Channel // the channels this person accepts
}

func (ct Contact) validate() error {
	switch {
	case ct.ID == "":
		return fmt.Errorf("escalation: a contact has no ID")
	case !ct.Role.valid():
		return fmt.Errorf("escalation: contact %q has an unknown role %q", ct.ID, ct.Role)
	case len(ct.Channels) == 0:
		return fmt.Errorf("escalation: contact %q accepts no channel", ct.ID)
	}
	chs := map[Channel]bool{}
	for _, ch := range ct.Channels {
		if !ch.valid() {
			return fmt.Errorf("escalation: contact %q has an unknown channel %q", ct.ID, ch)
		}
		if chs[ch] {
			return fmt.Errorf("escalation: contact %q lists channel %q twice", ct.ID, ch)
		}
		chs[ch] = true
	}
	return nil
}

// Audience selects the contacts a step tells.
type Audience string

const (
	// AudienceFirst is the deputies if there are any, else the owners.
	AudienceFirst    Audience = "first"
	AudienceOwner    Audience = "owner"
	AudienceGuard    Audience = "guard"
	AudienceRest     Audience = "rest" // managers and others
	AudienceEveryone Audience = "everyone"
)

func (a Audience) valid() bool {
	switch a {
	case AudienceFirst, AudienceOwner, AudienceGuard, AudienceRest, AudienceEveryone:
		return true
	}
	return false
}

// Wave is one step of a plan: at After (counted from the moment of the alert), tell
// To. A contact who was told before is told again: that is a reminder.
type Wave struct {
	After time.Duration
	To    []Audience
	// SMSNow sends the SMS together with the internet message instead of waiting for
	// the internet to fail. Fire and panic do: the delay costs too much.
	SMSNow bool
	// Double sends the GSM channels (SMS and call) GSMDelay after the internet
	// message when nobody answered, to insist.
	Double bool
}

// Repeat is the reminders that follow the last wave: everyone is told again by
// internet and SMS, Times times, Every apart.
type Repeat struct {
	Every time.Duration
	Times int
}

// Plan is who is told, and when, for one kind of alert. It is data, not code: the
// channel policy is adjustable per site and per kind.
type Plan struct {
	Waves  []Wave
	Repeat Repeat
	// InternetOnly keeps the alert off the GSM channels: it waits for the internet to
	// come back. GSMAfter, when positive, lets it use the GSM channels after that
	// long without any internet message getting through.
	InternetOnly bool
	GSMAfter     time.Duration
}

// Config holds the escalation settings.
type Config struct {
	SiteName string // in the texts
	Contacts []Contact
	Plans    map[AlertKind]Plan // missing kinds take the default plan

	// DeviceNames turns device identifiers into names for the texts.
	DeviceNames map[string]string
	Location    *time.Location // for the hour in the texts, nil means UTC (Lome)

	// InternetConfirm is how long the cloud has to confirm an internet message
	// before the GSM channels take over.
	InternetConfirm time.Duration
	// GSMDelay is the wait before the GSM channels double an unanswered internet
	// message.
	GSMDelay time.Duration
	// MaxPerHour is the most messages one contact gets per hour, to limit cost and
	// noise. Fire, panic and a silent siren are exempt.
	MaxPerHour int
}

// MaxContacts is the most contacts a site has.
const MaxContacts = 5

// DefaultPlans returns the plans decided in docs/escalation.md.
func DefaultPlans() map[AlertKind]Plan {
	reminders := Repeat{Every: 10 * time.Minute, Times: 3}
	oneFirst := Plan{Waves: []Wave{{To: []Audience{AudienceFirst}, Double: true}}}
	everyone := Plan{
		Waves:  []Wave{{To: []Audience{AudienceEveryone}, SMSNow: true, Double: true}},
		Repeat: reminders,
	}
	internetOwner := Plan{Waves: []Wave{{To: []Audience{AudienceOwner}}}, InternetOnly: true}
	return map[AlertKind]Plan{
		KindIntrusion: {
			Waves: []Wave{
				{To: []Audience{AudienceFirst, AudienceGuard}, Double: true},
				{After: 2 * time.Minute, To: []Audience{AudienceOwner, AudienceGuard, AudienceFirst}, Double: true},
				{After: 5 * time.Minute, To: []Audience{AudienceRest}, Double: true},
			},
			Repeat: reminders,
		},
		KindFire:           everyone,
		KindPanic:          everyone,
		KindTamper:         oneFirst,
		KindAuthFailed:     oneFirst,
		KindArmingBypassed: oneFirst,
		KindHubBatteryLow:  oneFirst,
		KindMainsRestored: {
			Waves:        []Wave{{To: []Audience{AudienceFirst}}},
			InternetOnly: true, GSMAfter: time.Hour,
		},
		KindDeviceOffline: internetOwner,
		KindBatteryLow:    internetOwner,
		KindGSMDegraded:   internetOwner,
		// The sensors cannot be heard: the alarm is blind, the SMS still works.
		KindZigbeeDown: oneFirst,
		// The siren is silent during an alarm: the guard, who can go and look, is
		// told with the first contact, once, under an incident of its own that names
		// the alarm. Before anyone handles the alarm, it is doubled by SMS and call
		// like the alarm, whose own plan goes on beside it and insists. Once the
		// alarm is handled, it is told without ringing (see quiet.go).
		KindSirenFailed: {Waves: []Wave{{To: []Audience{AudienceFirst, AudienceGuard}, Double: true}}},
	}
}

// DefaultConfig returns the defaults decided in docs/escalation.md.
func DefaultConfig(siteName string, contacts []Contact) Config {
	return Config{
		SiteName: siteName, Contacts: contacts, Plans: DefaultPlans(),
		InternetConfirm: 10 * time.Second, GSMDelay: time.Minute, MaxPerHour: 20,
	}
}

// Validate checks that the configuration is usable.
func (c Config) Validate() error {
	if c.SiteName == "" {
		return fmt.Errorf("escalation: SiteName is required")
	}
	if len(c.Contacts) > MaxContacts {
		return fmt.Errorf("escalation: at most %d contacts, got %d", MaxContacts, len(c.Contacts))
	}
	seen := map[string]bool{}
	owner := false
	for _, ct := range c.Contacts {
		if seen[ct.ID] {
			return fmt.Errorf("escalation: contact %q appears twice", ct.ID)
		}
		if err := ct.validate(); err != nil {
			return err
		}
		seen[ct.ID] = true
		owner = owner || ct.Role == RoleOwner
	}
	if len(c.Contacts) > 0 && !owner {
		return fmt.Errorf("escalation: a site needs an owner among its contacts")
	}
	if c.InternetConfirm <= 0 || c.GSMDelay <= 0 {
		return fmt.Errorf("escalation: InternetConfirm and GSMDelay must be positive")
	}
	if c.MaxPerHour <= 0 {
		return fmt.Errorf("escalation: MaxPerHour must be positive")
	}
	for kind, p := range c.Plans {
		if !slices.Contains(allKinds, kind) {
			return fmt.Errorf("escalation: plan for an unknown kind %q", kind)
		}
		if err := p.Validate(); err != nil {
			return fmt.Errorf("escalation: plan %q: %w", kind, err)
		}
	}
	return nil
}

// totalSteps is the number of steps of a plan: its waves, then its reminders.
func (p Plan) totalSteps() int { return len(p.Waves) + p.Repeat.Times }

// stepAt is when step i is due, counted from the moment of the alert.
func (p Plan) stepAt(i int) time.Duration {
	if i < len(p.Waves) {
		return p.Waves[i].After
	}
	return p.Waves[len(p.Waves)-1].After + time.Duration(i-len(p.Waves)+1)*p.Repeat.Every
}

// step describes what step i does. A reminder is a wave to everyone, by internet
// and SMS.
func (p Plan) step(i int) Wave {
	if i < len(p.Waves) {
		return p.Waves[i]
	}
	return Wave{To: []Audience{AudienceEveryone}, SMSNow: true}
}

// Validate checks a plan on its own.
func (p Plan) Validate() error {
	if len(p.Waves) == 0 {
		return fmt.Errorf("at least one wave is required")
	}
	var prev time.Duration
	for i, w := range p.Waves {
		switch {
		case w.After < prev:
			return fmt.Errorf("wave %d starts before the previous one", i+1)
		case len(w.To) == 0:
			return fmt.Errorf("wave %d tells nobody", i+1)
		}
		prev = w.After
		for _, a := range w.To {
			if !a.valid() {
				return fmt.Errorf("wave %d: unknown audience %q", i+1, a)
			}
		}
	}
	if p.Repeat.Times < 0 || (p.Repeat.Times > 0 && p.Repeat.Every <= 0) {
		return fmt.Errorf("reminders need a positive interval")
	}
	if p.GSMAfter < 0 || (p.GSMAfter > 0 && !p.InternetOnly) {
		return fmt.Errorf("GSMAfter only applies to an internet-only plan and must not be negative")
	}
	return nil
}

func (c Config) contact(id string) (Contact, bool) {
	for _, ct := range c.Contacts {
		if ct.ID == id {
			return ct, true
		}
	}
	return Contact{}, false
}

// audience lists the contacts an audience designates, in configuration order. A
// role may be held by several contacts (two co-owners, two deputies): an audience
// that names a role tells all of them, or the second of a couple would only hear of
// an intrusion at the first reminder, and never of a tamper.
func (c Config) audience(a Audience) []Contact {
	if a == AudienceFirst {
		if deputies := c.withRole(RoleDeputy); len(deputies) > 0 {
			return deputies
		}
		return c.withRole(RoleOwner)
	}
	var out []Contact
	for _, ct := range c.Contacts {
		switch {
		case a == AudienceEveryone,
			a == AudienceOwner && ct.Role == RoleOwner,
			a == AudienceGuard && ct.Role == RoleGuard,
			a == AudienceRest && (ct.Role == RoleManager || ct.Role == RoleOther):
			out = append(out, ct)
		}
	}
	return out
}

func (c Config) withRole(r Role) []Contact {
	var out []Contact
	for _, ct := range c.Contacts {
		if ct.Role == r {
			out = append(out, ct)
		}
	}
	return out
}
