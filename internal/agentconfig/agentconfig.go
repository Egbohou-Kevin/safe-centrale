// Package agentconfig reads the link agent's configuration file (JSON), strictly: a
// misspelt key is an error. The password for the cloud is in a file of its own, so
// that the configuration can be shown or copied without it. See
// config/agent.example.json.
package agentconfig

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"

	"safehome/internal/agent"
	"safehome/internal/agentapp"
	"safehome/internal/configfile"
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
	configfile.Program // BrokerURL is the hub's broker
	App                agentapp.Config
	Cloud              Cloud
	PhotoDir           string // the photos the video service took, for the agent to send
	KeyDir             string // the agent's own folder, KeyFolder in the state directory
	// Next is the cloud of a move under way (NextName in KeyDir), nil if none: the
	// agent tries it until the move ends (see move.go).
	Next *Cloud
	// NextRef is the agent.move that wrote Next, MovedRef the one that wrote the
	// current cloud ("" if the file's): after a stop, they tell the agent where its
	// move stands.
	NextRef, MovedRef string
}

// KeyFolder is the agent's own folder in the state directory, made 0700 and hidden from the
// other programs' units: it holds the key for the sealed codes.
const KeyFolder = "agent"

type file struct {
	configfile.ProgramFile
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
	var f file
	if err := configfile.Decode(path, &f); err != nil {
		return Result{}, fmt.Errorf("agentconfig: %w", err)
	}
	r, err := build(f, filepath.Dir(path))
	if err != nil {
		return Result{}, fmt.Errorf("agentconfig: %s: %w", path, err)
	}
	return r, nil
}

func build(f file, dir string) (Result, error) {
	p, err := f.Build("safehome-agent-", "where the agent keeps its queue")
	if err != nil {
		return Result{}, err
	}
	r := Result{
		Program:  p,
		PhotoDir: filepath.Join(f.StateDir, "photos"), // the same folder as the video service's (videoconfig)
		KeyDir:   filepath.Join(f.StateDir, KeyFolder),
	}
	if r.Cloud, err = cloud(f.Cloud, f.Site, dir); err != nil {
		return Result{}, err
	}
	// A cloud the agent moved to overrides the file's (move.go).
	moved, movedRef, err := readMoved(r.KeyDir, MovedName, r.Cloud)
	if err != nil {
		return Result{}, err
	}
	if moved != nil {
		r.Cloud, r.MovedRef = *moved, movedRef
	}
	if r.Next, r.NextRef, err = readMoved(r.KeyDir, NextName, r.Cloud); err != nil {
		return Result{}, err
	}
	if r.App, err = appConfig(f, r.Cloud.URLs); err != nil {
		return Result{}, err
	}
	return r, nil
}

// appConfig reads the agent's settings over the defaults. The photos go only to the
// hosts of cloudURLs.
func appConfig(f file, cloudURLs []string) (agentapp.Config, error) {
	app := agentapp.DefaultConfig(f.Site)
	app.Relay.CloudURLs = cloudURLs
	if err := configfile.Duration("status_every", f.StatusEvery, &app.StatusEvery); err != nil {
		return agentapp.Config{}, err
	}
	if err := configfile.Duration("confirm_wait", f.ConfirmWait, &app.Relay.ConfirmWait); err != nil {
		return agentapp.Config{}, err
	}
	if q := f.Queue; q != nil {
		setInt(&app.Relay.QueueItems, q.MaxItems)
		setInt(&app.Relay.QueueBytes, q.MaxBytes)
		if err := configfile.Duration("queue.max_age", q.MaxAge, &app.Relay.QueueAge); err != nil {
			return agentapp.Config{}, err
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
		return agentapp.Config{}, fmt.Errorf(`uplink %q: use "fixed" (the shop's box) or "mobile" (a 4G router)`, f.Uplink)
	}
	return app, app.Validate()
}

// cloud reads how to reach the cloud. The connection is always encrypted: mqtts or
// wss, never plain mqtt or ws.
func cloud(f *cloudFile, site, dir string) (Cloud, error) {
	if f == nil || len(f.URLs) == 0 {
		return Cloud{}, errors.New("cloud.urls is required (the cloud's broker, then its fallbacks)")
	}
	for i, raw := range f.URLs {
		if err := checkCloudURL(raw); err != nil {
			return Cloud{}, fmt.Errorf("cloud.urls[%d]: %w", i, err)
		}
	}
	c := Cloud{
		URLs:     f.URLs,
		ClientID: cmp.Or(f.ClientID, "safehome-agent-"+site),
		Username: cmp.Or(f.Username, site),
	}
	if f.PasswordFile == "" {
		return Cloud{}, errors.New("cloud.password_file is required (the site's secret for the cloud)")
	}
	var err error
	if c.Password, err = configfile.Secret(dir, f.PasswordFile); err != nil {
		return Cloud{}, fmt.Errorf("cloud.password_file: %w", err)
	}
	if c.Password == "" {
		return Cloud{}, errors.New("cloud.password_file is empty")
	}
	if f.CAFile != "" {
		c.CAFile = configfile.Path(dir, f.CAFile)
		if _, err := os.Stat(c.CAFile); err != nil {
			return Cloud{}, fmt.Errorf("cloud.ca_file: %w", err)
		}
	}
	return c, nil
}

// checkCloudURL accepts an encrypted address without credentials. The errors never
// repeat a password written in it by mistake: they end up in the logs.
func checkCloudURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return errors.New("invalid URL (mqtts://host:8883 or wss://host:443/mqtt)")
	case u.Host == "":
		return fmt.Errorf("invalid URL %q", u.Redacted())
	case u.Scheme != "mqtts" && u.Scheme != "wss":
		return fmt.Errorf("%q is not encrypted, use mqtts:// or wss://", u.Redacted())
	case u.User != nil:
		return fmt.Errorf("no credentials in %q, use username and password_file", u.Redacted())
	}
	return nil
}

func setInt(dst *int, v *int) {
	if v != nil {
		*dst = *v
	}
}
