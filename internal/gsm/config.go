package gsm

import (
	"fmt"
	"strings"
	"time"
)

// Line is a modem and its SIM.
type Line struct {
	ID       string
	Operator string // as configured, until the modem says
}

// Config holds the gateway settings.
type Config struct {
	Lines []Line // in order of preference
	// Contacts gives each contact's phone number. Numbers are normalised to the
	// international format at start.
	Contacts map[string]string
	Country  string // calling code for local numbers, "228" for Togo

	RingFor         time.Duration // how long a call rings
	CheckEvery      time.Duration // line check interval
	TestEvery       time.Duration // test SMS interval
	TestNumber      string        // where the test SMS goes, empty for none
	MissesToRestart int           // checks without answer before restarting the modem

	MaxAge    map[Priority]time.Duration // a request older than this is given up
	RetryMin  time.Duration
	RetryMax  time.Duration
	AckWindow time.Duration // how long after a message a "1" acknowledges it

	SMSTimeout     time.Duration // a line that does not answer by then failed
	CheckTimeout   time.Duration
	RestartTimeout time.Duration
}

// DefaultConfig returns the defaults decided in docs/gsm-gateway.md.
func DefaultConfig(lines []Line, contacts map[string]string) Config {
	return Config{
		Lines: lines, Contacts: contacts, Country: "228",
		RingFor: 20 * time.Second, CheckEvery: 30 * time.Second, TestEvery: 7 * 24 * time.Hour,
		MissesToRestart: 3,
		MaxAge: map[Priority]time.Duration{
			PriorityLifeSafety: 10 * time.Minute, PriorityIntrusion: 10 * time.Minute, PriorityOther: time.Hour,
		},
		RetryMin: 5 * time.Second, RetryMax: 2 * time.Minute, AckWindow: 24 * time.Hour,
		SMSTimeout: 90 * time.Second, CheckTimeout: 30 * time.Second, RestartTimeout: 3 * time.Minute,
	}
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if len(c.Lines) == 0 {
		return fmt.Errorf("gsm: at least one line is required")
	}
	seen := map[string]bool{}
	for _, l := range c.Lines {
		if l.ID == "" || seen[l.ID] {
			return fmt.Errorf("gsm: line ids must be set and unique")
		}
		seen[l.ID] = true
	}
	if !digits(c.Country) || len(c.Country) > 3 {
		return fmt.Errorf("gsm: invalid country code %q", c.Country)
	}
	for contact, number := range c.Contacts {
		if contact == "" {
			return fmt.Errorf("gsm: a contact has no id")
		}
		if _, err := NormalizeNumber(number, c.Country); err != nil {
			return fmt.Errorf("gsm: contact %q: %w", contact, err)
		}
	}
	if c.TestNumber != "" {
		if _, err := NormalizeNumber(c.TestNumber, c.Country); err != nil {
			return fmt.Errorf("gsm: test number: %w", err)
		}
	}
	for name, d := range map[string]time.Duration{
		"RingFor": c.RingFor, "CheckEvery": c.CheckEvery, "TestEvery": c.TestEvery, "RetryMin": c.RetryMin,
		"AckWindow": c.AckWindow, "SMSTimeout": c.SMSTimeout, "CheckTimeout": c.CheckTimeout, "RestartTimeout": c.RestartTimeout,
	} {
		if d <= 0 {
			return fmt.Errorf("gsm: %s must be positive", name)
		}
	}
	if c.RetryMax < c.RetryMin {
		return fmt.Errorf("gsm: RetryMax must not be below RetryMin")
	}
	if c.MissesToRestart < 1 {
		return fmt.Errorf("gsm: MissesToRestart must be at least 1")
	}
	for _, p := range []Priority{PriorityLifeSafety, PriorityIntrusion, PriorityOther} {
		if c.MaxAge[p] <= 0 {
			return fmt.Errorf("gsm: a maximum age is required for every priority")
		}
	}
	return nil
}

// NormalizeNumber writes a phone number in the international format (+228...).
// Spaces, dots, dashes and brackets are ignored; "00" stands for "+"; a number
// without a country code is local.
func NormalizeNumber(raw, country string) (string, error) {
	s := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '.', '-', '(', ')', '\t':
			return -1
		}
		return r
	}, raw)
	switch {
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	case strings.HasPrefix(s, "00"):
		s = s[2:]
	default:
		s = strings.TrimPrefix(s, "0") // a trunk prefix, where one is used
		s = country + s
	}
	if !digits(s) || len(s) < 8 || len(s) > 15 {
		return "", fmt.Errorf("invalid phone number %q", raw)
	}
	return "+" + s, nil
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
