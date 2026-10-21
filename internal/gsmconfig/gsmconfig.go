// Package gsmconfig reads the GSM gateway's configuration file (JSON), strictly: a
// misspelt key is an error. See config/gsm.example.json.
package gsmconfig

import (
	"cmp"
	"fmt"
	"time"

	"safehome/internal/configfile"
	"safehome/internal/gateway"
	"safehome/internal/gsm"
	"safehome/internal/siteconfig"
)

// Port is where a line's modem is.
type Port struct {
	Path      string // the serial device, for example /dev/ttyUSB2
	Baud      int
	Configure bool // set the port up with stty before opening it
}

// Result is what the file describes.
type Result struct {
	configfile.Program
	Gateway gateway.Config
	Ports   map[string]Port // line -> port

	// SiteVersion is the version of the configuration set from the cloud found in
	// the state folder (0: none); SiteErr says why it was not applied (the gateway
	// then uses its own file alone).
	SiteVersion int
	SiteErr     error
}

type lineFile struct {
	ID        string `json:"id"`
	Port      string `json:"port"`
	Baud      int    `json:"baud"`
	Operator  string `json:"operator"`
	Configure *bool  `json:"configure_port"`
}

type maxAgeFile struct {
	LifeSafety string `json:"life_safety"`
	Intrusion  string `json:"intrusion"`
	Other      string `json:"other"`
}

type file struct {
	configfile.ProgramFile
	Country    string            `json:"country"`
	Lines      []lineFile        `json:"lines"`
	Contacts   map[string]string `json:"contacts"`
	TestNumber string            `json:"test_number"`
	RingFor    string            `json:"ring_for"`
	CheckEvery string            `json:"check_every"`
	TestEvery  string            `json:"test_every"`
	// StatusEvery must stay well under the hub's health.gsm_silent_after.
	StatusEvery string      `json:"status_every"`
	MaxAge      *maxAgeFile `json:"max_age"`
}

// Load reads and checks the file at path, then takes the contacts' phone numbers
// from the configuration set from the cloud, if the state folder holds one
// (internal/siteconfig). One that does not fit never stops the gateway: SiteErr
// says why it was left aside.
func Load(path string) (Result, error) {
	var f file
	if err := configfile.Decode(path, &f); err != nil {
		return Result{}, fmt.Errorf("gsmconfig: %w", err)
	}
	r, err := build(f)
	if err != nil {
		return Result{}, fmt.Errorf("gsmconfig: %s: %w", path, err)
	}
	return withSite(r, f), nil
}

// withSite lays the cloud's contacts over the file's: the contacts with a phone
// number, and no other, can be reached by SMS and call. Every contact, those reached
// by the app only too, goes to Gateway.Contacts: an acknowledgement from any of them
// ends the incident for the escalation, so it does for the gateway.
func withSite(r Result, f file) Result {
	o, ok, err := siteconfig.Read(r.StateDir)
	if err != nil || !ok {
		r.SiteErr = err
		return r
	}
	var all []string
	if o.Contacts != nil {
		f.Contacts = map[string]string{}
		all = make([]string, 0, len(o.Contacts))
		for _, c := range o.Contacts {
			if c.Phone != "" {
				f.Contacts[c.ID] = c.Phone
			}
			all = append(all, c.ID)
		}
	}
	merged, err := build(f)
	if err != nil {
		r.SiteVersion, r.SiteErr = o.Version, err
		return r
	}
	merged.SiteVersion = o.Version
	merged.Gateway.Contacts = all
	return merged
}

func build(f file) (Result, error) {
	p, err := f.Build("safehome-gsm-", "where the gateway keeps its queue")
	if err != nil {
		return Result{}, err
	}
	r := Result{Program: p}
	lines, ports, err := parseLines(f.Lines)
	if err != nil {
		return Result{}, err
	}
	r.Ports = ports
	g := gsm.DefaultConfig(lines, f.Contacts)
	g.Country = cmp.Or(f.Country, g.Country)
	g.TestNumber = f.TestNumber
	if err := parseDurations(f, &g); err != nil {
		return Result{}, err
	}
	r.Gateway = gateway.DefaultConfig(f.Site, g)
	if err := configfile.Duration("status_every", f.StatusEvery, &r.Gateway.StatusEvery); err != nil {
		return Result{}, err
	}
	if err := r.Gateway.Validate(); err != nil {
		return Result{}, err
	}
	return r, nil
}

// parseLines returns the lines, in order of preference, and their ports.
func parseLines(in []lineFile) ([]gsm.Line, map[string]Port, error) {
	var lines []gsm.Line
	ports := map[string]Port{}
	for _, l := range in {
		if l.Port == "" {
			return nil, nil, fmt.Errorf("line %q: port is required", l.ID)
		}
		if l.Baud < 0 {
			return nil, nil, fmt.Errorf("line %q: baud must be positive", l.ID)
		}
		baud := cmp.Or(l.Baud, 115200)
		configure := l.Configure == nil || *l.Configure
		ports[l.ID] = Port{Path: l.Port, Baud: baud, Configure: configure}
		lines = append(lines, gsm.Line{ID: l.ID, Operator: l.Operator})
	}
	return lines, ports, nil
}

// parseDurations lays the file's durations over the defaults in g.
func parseDurations(f file, g *gsm.Config) error {
	for _, d := range []struct {
		name, value string
		dst         *time.Duration
	}{
		{"ring_for", f.RingFor, &g.RingFor},
		{"check_every", f.CheckEvery, &g.CheckEvery},
		{"test_every", f.TestEvery, &g.TestEvery},
	} {
		if err := configfile.Duration(d.name, d.value, d.dst); err != nil {
			return err
		}
	}
	if f.MaxAge == nil {
		return nil
	}
	for _, d := range []struct {
		name, value string
		p           gsm.Priority
	}{
		{"max_age.life_safety", f.MaxAge.LifeSafety, gsm.PriorityLifeSafety},
		{"max_age.intrusion", f.MaxAge.Intrusion, gsm.PriorityIntrusion},
		{"max_age.other", f.MaxAge.Other, gsm.PriorityOther},
	} {
		v := g.MaxAge[d.p]
		if err := configfile.Duration(d.name, d.value, &v); err != nil {
			return err
		}
		g.MaxAge[d.p] = v
	}
	return nil
}
