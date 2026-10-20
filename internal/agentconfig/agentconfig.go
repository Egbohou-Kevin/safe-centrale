// Package agentconfig reads the link agent's configuration file (JSON), strictly: a
// misspelt key is an error. The password for the cloud is in a file of its own, so
// that the configuration can be shown or copied without it. See
// config/agent.example.json.
package agentconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"safehome/internal/agent"
	"safehome/internal/agentapp"
)

// Cloud is how to reach the cloud's broker.
type Cloud struct {
	URLs     []string // the first, then the fallbacks: "mqtts://...:8883", "wss://...:443/mqtt"
	ClientID string
	Username string
	Password string // read from password_file; never logged
	CAFile   string // empty: the system's authorities
}

// String hides the password.
func (c Cloud) String() string {
	return fmt.Sprintf("{urls:%v client_id:%q username:%q password:*** ca_file:%q}", c.URLs, c.ClientID, c.Username, c.CAFile)
}

// GoString hides the password for %#v.
func (c Cloud) GoString() string { return c.String() }

// LogValue hides the password in slog logs.
func (c Cloud) LogValue() slog.Value {
	return slog.GroupValue(slog.Any("urls", c.URLs), slog.String("client_id", c.ClientID), slog.String("username", c.Username))
}

// Result is what the file describes.
type Result struct {
	App       agentapp.Config
	Cloud     Cloud
	BrokerURL string // the hub's broker
	ClientID  string
	StateDir  string
	PhotoDir  string // the photos the video service took, for the agent to send
	LogLevel  slog.Level
}

type file struct {
	Site        string     `json:"site"`
	BrokerURL   string     `json:"broker_url"`
	ClientID    string     `json:"client_id"`
	StateDir    string     `json:"state_dir"`
	LogLevel    string     `json:"log_level"`
	Uplink      string     `json:"uplink"`
	Cloud       *cloudFile `json:"cloud"`
	StatusEvery string     `json:"status_every"`
	ConfirmWait string     `json:"confirm_wait"`
	Queue       *queueFile `json:"queue"`
	Down        *downFile  `json:"down"`
}

type cloudFile struct {
	URLs         []string `json:"urls"`
	ClientID     string   `json:"client_id"`
	Username     string   `json:"username"`
	PasswordFile string   `json:"password_file"`
	CAFile       string   `json:"ca_file"`
}

type queueFile struct {
	MaxItems *int   `json:"max_items"`
	MaxBytes *int   `json:"max_bytes"`
	MaxAge   string `json:"max_age"`
}

type downFile struct {
	Burst     *int `json:"burst"`
	PerMinute *int `json:"per_minute"`
}

// Load reads the configuration at path. Relative password_file and ca_file are taken
// from the configuration file's folder.
func Load(path string) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("agentconfig: %w", err)
	}
	r, err := parse(data, filepath.Dir(path))
	if err != nil {
		return Result{}, fmt.Errorf("agentconfig: %s: %w", path, err)
	}
	return r, nil
}

func parse(data []byte, dir string) (Result, error) {
	var f file
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Result{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if dec.More() {
		return Result{}, errors.New("invalid JSON: trailing data")
	}
	if f.StateDir == "" {
		return Result{}, errors.New("state_dir is required (where the agent keeps its queue)")
	}
	// The same folder as the video service's (videoconfig).
	r := Result{BrokerURL: f.BrokerURL, ClientID: f.ClientID, StateDir: f.StateDir, PhotoDir: filepath.Join(f.StateDir, "photos")}
	if r.BrokerURL == "" {
		r.BrokerURL = "mqtt://127.0.0.1:1883"
	}
	if r.ClientID == "" {
		r.ClientID = "safehome-agent-" + f.Site
	}
	level, err := parseLevel(f.LogLevel)
	if err != nil {
		return Result{}, err
	}
	r.LogLevel = level

	if r.Cloud, err = cloud(f.Cloud, f.Site, dir); err != nil {
		return Result{}, err
	}

	app := agentapp.DefaultConfig(f.Site)
	if err := duration("status_every", f.StatusEvery, &app.StatusEvery); err != nil {
		return Result{}, err
	}
	if err := duration("confirm_wait", f.ConfirmWait, &app.Relay.ConfirmWait); err != nil {
		return Result{}, err
	}
	if q := f.Queue; q != nil {
		setInt(&app.Relay.QueueItems, q.MaxItems)
		setInt(&app.Relay.QueueBytes, q.MaxBytes)
		if err := duration("queue.max_age", q.MaxAge, &app.Relay.QueueAge); err != nil {
			return Result{}, err
		}
	}
	if d := f.Down; d != nil {
		setInt(&app.Relay.DownBurst, d.Burst)
		setInt(&app.Relay.DownPerMinute, d.PerMinute)
	}
	switch f.Uplink {
	case "", "fixed":
	case "mobile":
		// Economy: the data is paid by the client (as in videoconfig).
		app.Relay.Economy = agent.EconomyEvery
	default:
		return Result{}, fmt.Errorf(`uplink %q: use "fixed" (the shop's box) or "mobile" (a 4G router)`, f.Uplink)
	}
	if err := app.Validate(); err != nil {
		return Result{}, err
	}
	r.App = app
	return r, nil
}

// cloud reads how to reach the cloud. The connection is always encrypted: mqtts or
// wss, never plain mqtt or ws.
func cloud(f *cloudFile, site, dir string) (Cloud, error) {
	if f == nil || len(f.URLs) == 0 {
		return Cloud{}, errors.New("cloud.urls is required (the cloud's broker, then its fallbacks)")
	}
	c := Cloud{URLs: f.URLs, ClientID: f.ClientID, Username: f.Username}
	for _, raw := range f.URLs {
		u, err := url.Parse(raw)
		switch {
		case err != nil || u.Host == "":
			return Cloud{}, fmt.Errorf("cloud.urls: invalid URL %q", raw)
		case u.Scheme != "mqtts" && u.Scheme != "wss":
			return Cloud{}, fmt.Errorf("cloud.urls: %q is not encrypted, use mqtts:// or wss://", raw)
		case u.User != nil:
			return Cloud{}, fmt.Errorf("cloud.urls: no credentials in %q, use username and password_file", u.Redacted())
		}
	}
	if c.ClientID == "" {
		c.ClientID = "safehome-agent-" + site
	}
	if c.Username == "" {
		c.Username = site
	}
	if f.PasswordFile == "" {
		return Cloud{}, errors.New("cloud.password_file is required (the site's secret for the cloud)")
	}
	pw, err := os.ReadFile(relative(dir, f.PasswordFile))
	if err != nil {
		return Cloud{}, fmt.Errorf("cloud.password_file: %w", err)
	}
	c.Password = strings.TrimSpace(string(pw))
	if c.Password == "" {
		return Cloud{}, errors.New("cloud.password_file is empty")
	}
	if f.CAFile != "" {
		c.CAFile = relative(dir, f.CAFile)
		if _, err := os.Stat(c.CAFile); err != nil {
			return Cloud{}, fmt.Errorf("cloud.ca_file: %w", err)
		}
	}
	return c, nil
}

func relative(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
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

func setInt(dst *int, v *int) {
	if v != nil {
		*dst = *v
	}
}
