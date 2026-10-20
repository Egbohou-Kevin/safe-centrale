// Package gsmconfig reads the GSM gateway's configuration file (JSON), strictly: a
// misspelt key is an error. See config/gsm.example.json.
package gsmconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

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
	Gateway   gateway.Config
	Ports     map[string]Port // line -> port
	BrokerURL string
	ClientID  string
	StateDir  string
	LogLevel  slog.Level

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
	Site       string            `json:"site"`
	BrokerURL  string            `json:"broker_url"`
	ClientID   string            `json:"client_id"`
	StateDir   string            `json:"state_dir"`
	LogLevel   string            `json:"log_level"`
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
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("gsmconfig: %w", err)
	}
	f, err := decode(data)
	if err == nil {
		var r Result
		if r, err = build(f); err == nil {
			return withSite(r, f), nil
		}
	}
	return Result{}, fmt.Errorf("gsmconfig: %s: %w", path, err)
}

// withSite lays the cloud's contacts over the file's: the contacts with a phone
// number, and no other, can be reached by SMS and call.
func withSite(r Result, f file) Result {
	o, ok, err := siteconfig.Read(r.StateDir)
	if err != nil || !ok {
		r.SiteErr = err
		return r
	}
	if o.Contacts != nil {
		f.Contacts = map[string]string{}
		for _, c := range o.Contacts {
			if c.Phone != "" {
				f.Contacts[c.ID] = c.Phone
			}
		}
	}
	merged, err := build(f)
	if err != nil {
		r.SiteVersion, r.SiteErr = o.Version, err
		return r
	}
	merged.SiteVersion = o.Version
	return merged
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
		return Result{}, errors.New("state_dir is required (where the gateway keeps its queue)")
	}
	r := Result{BrokerURL: f.BrokerURL, ClientID: f.ClientID, StateDir: f.StateDir, Ports: map[string]Port{}}
	if r.BrokerURL == "" {
		r.BrokerURL = "mqtt://127.0.0.1:1883"
	}
	if r.ClientID == "" {
		r.ClientID = "safehome-gsm-" + f.Site
	}
	level, err := parseLevel(f.LogLevel)
	if err != nil {
		return Result{}, err
	}
	r.LogLevel = level

	var lines []gsm.Line
	for _, l := range f.Lines {
		if l.Port == "" {
			return Result{}, fmt.Errorf("line %q: port is required", l.ID)
		}
		baud := l.Baud
		if baud == 0 {
			baud = 115200
		}
		configure := l.Configure == nil || *l.Configure
		r.Ports[l.ID] = Port{Path: l.Port, Baud: baud, Configure: configure}
		lines = append(lines, gsm.Line{ID: l.ID, Operator: l.Operator})
	}
	g := gsm.DefaultConfig(lines, f.Contacts)
	if f.Country != "" {
		g.Country = f.Country
	}
	g.TestNumber = f.TestNumber
	for _, d := range []struct {
		name, value string
		dst         *time.Duration
	}{
		{"ring_for", f.RingFor, &g.RingFor},
		{"check_every", f.CheckEvery, &g.CheckEvery},
		{"test_every", f.TestEvery, &g.TestEvery},
	} {
		if err := duration(d.name, d.value, d.dst); err != nil {
			return Result{}, err
		}
	}
	if f.MaxAge != nil {
		for _, d := range []struct {
			name, value string
			p           gsm.Priority
		}{
			{"max_age.life_safety", f.MaxAge.LifeSafety, gsm.PriorityLifeSafety},
			{"max_age.intrusion", f.MaxAge.Intrusion, gsm.PriorityIntrusion},
			{"max_age.other", f.MaxAge.Other, gsm.PriorityOther},
		} {
			v := g.MaxAge[d.p]
			if err := duration(d.name, d.value, &v); err != nil {
				return Result{}, err
			}
			g.MaxAge[d.p] = v
		}
	}
	r.Gateway = gateway.DefaultConfig(f.Site, g)
	if err := duration("status_every", f.StatusEvery, &r.Gateway.StatusEvery); err != nil {
		return Result{}, err
	}
	if err := r.Gateway.Validate(); err != nil {
		return Result{}, err
	}
	return r, nil
}

func duration(name, value string, dst *time.Duration) error {
	if value == "" {
		return nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s %q: %w (write for example 20s, 30s or 168h)", name, value, err)
	}
	*dst = d
	return nil
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
