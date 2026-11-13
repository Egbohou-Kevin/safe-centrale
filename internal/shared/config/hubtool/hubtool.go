// Package hubtool is the contract between the hub and the installer's tools (the
// command-line tool, the local console): what they may know of the hub's
// configuration, and the form of the zones and sirens they read and write. The hub
// alone reads its configuration and checks it, by its own rules; the tools ask it by
// its command line (safehome-hub site), so a rule is never written twice and no
// program imports another (docs/rangement-par-programme.md, 13/11/2026).
package hubtool

import (
	"encoding/json"
	"fmt"
	"time"
	_ "time/tzdata" // the hub's image may have no time zone database

	"safehome/internal/shared/config/configfile"
	"safehome/internal/shared/core/access"
	sitemodel "safehome/internal/shared/core/site"
)

// The command line the hub answers: safehome-hub site -config FILE VERB. Read and
// view print JSON on the standard output; check and write read the site in JSON on
// the standard input. A refusal exits 1 and says why on the standard error.
const (
	Command = "site"
	View    = "view"  // the hub's configuration as the tools need it (Config)
	Read    = "read"  // the zones and sirens the hub runs with (Site)
	Check   = "check" // whether the hub would accept a site, writing nothing
	Write   = "write" // keep a site in the hub's state folder, once checked
)

// PidFile is the hub's pid file in its state folder. The installer's tools signal
// the process it names to have the access codes read again.
const PidFile = "hub.pid"

// Config is what the tools know of the hub's configuration, its rules applied.
type Config struct {
	Site      string    `json:"site"`
	StateDir  string    `json:"state_dir"`
	BrokerURL string    `json:"broker_url"`
	Timezone  string    `json:"timezone"` // IANA name; empty: UTC
	Contacts  []Contact `json:"contacts"`
	Access    Access    `json:"access"`
}

// Contact is a person of the site, as the codes' pages show it.
type Contact struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// Access is the rule of the hub's access codes.
type Access struct {
	MaxUsers   int `json:"max_users"`
	MinDigits  int `json:"min_digits"`
	MaxDigits  int `json:"max_digits"`
	Iterations int `json:"iterations"`
}

// AccessOf is the rule as the hub gives it.
func AccessOf(c access.Config) Access {
	return Access{MaxUsers: c.MaxUsers, MinDigits: c.MinDigits, MaxDigits: c.MaxDigits, Iterations: c.Iterations}
}

// Config is the rule, salted from the system's secure random source.
func (a Access) Config() access.Config {
	return access.Config{MaxUsers: a.MaxUsers, MinDigits: a.MinDigits, MaxDigits: a.MaxDigits, Iterations: a.Iterations}
}

// Location is the site's time zone, nil for UTC (the hub's own reading).
func (c Config) Location() (*time.Location, error) {
	if c.Timezone == "" {
		return nil, nil
	}
	return time.LoadLocation(c.Timezone)
}

// AsOperator is the operator on the hub's broker, for a tool run by hand under
// sudo: the operator's password is in the file at path (configfile.OperatorSecret
// when empty).
func (c Config) AsOperator(path string) (configfile.Program, error) {
	return configfile.Program{BrokerURL: c.BrokerURL, StateDir: c.StateDir}.AsOperator(path)
}

// Site is the form of the zones and sirens on the command line.
type Site struct {
	Zones      map[string]Zone   `json:"zones"`
	SirenIDs   []string          `json:"siren_ids"`
	SirenNames map[string]string `json:"siren_names,omitempty"`
}

// Zone is one zone of Site.
type Zone struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Room string `json:"room,omitempty"`
}

// SiteOf is the form of s.
func SiteOf(s sitemodel.Site) Site {
	out := Site{Zones: make(map[string]Zone, len(s.Zones)), SirenIDs: s.SirenIDs, SirenNames: s.SirenNames}
	for id, z := range s.Zones {
		out.Zones[id] = Zone(z)
	}
	if out.SirenIDs == nil {
		out.SirenIDs = []string{}
	}
	return out
}

// Model is the site s describes.
func (s Site) Model() sitemodel.Site {
	out := sitemodel.Site{Zones: make(map[string]sitemodel.Zone, len(s.Zones)), SirenIDs: s.SirenIDs, SirenNames: s.SirenNames}
	for id, z := range s.Zones {
		out.Zones[id] = sitemodel.Zone(z)
	}
	return out
}

// Hub asks the hub by its command line: Call runs safehome-hub command -config FILE
// verb with in on its standard input, and returns its standard output
// (internal/shared/adapter/hubexec).
type Hub struct {
	Call func(command, verb string, in []byte) ([]byte, error)
}

// View is the hub's configuration, as the tools need it.
func (h Hub) View() (Config, error) {
	var c Config
	return c, h.ask(View, nil, &c)
}

// Read returns the zones and sirens the hub runs with.
func (h Hub) Read() (sitemodel.Site, error) {
	var s Site
	if err := h.ask(Read, nil, &s); err != nil {
		return sitemodel.Site{}, err
	}
	return s.Model(), nil
}

// Check says whether the hub would accept s, writing nothing.
func (h Hub) Check(s sitemodel.Site) error { return h.send(Check, s) }

// Write keeps s in the hub's state folder, once the hub checked it.
func (h Hub) Write(s sitemodel.Site) error { return h.send(Write, s) }

func (h Hub) send(verb string, s sitemodel.Site) error {
	in, err := json.Marshal(SiteOf(s))
	if err != nil {
		return fmt.Errorf("hubtool: %w", err)
	}
	return h.ask(verb, in, nil)
}

// ask decodes the hub's answer to verb into out (nil: none expected), strictly.
func (h Hub) ask(verb string, in []byte, out any) error {
	data, err := h.Call(Command, verb, in)
	if err != nil || out == nil {
		return err
	}
	if err := configfile.DecodeBytes(data, out); err != nil {
		return fmt.Errorf("hubtool: the hub's answer: %w", err)
	}
	return nil
}
