// Package maintconfig reads the remote maintenance service's configuration file
// (JSON), strictly: a misspelt key is an error. See config/maint.example.json and
// docs/maintenance.md.
package maintconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"safehome/internal/maint"
	"safehome/internal/maintapp"
)

// Tunnel says how to reach the bastion.
type Tunnel struct {
	SSH        string // the OpenSSH client
	Host       string
	Port       int
	User       string
	KeyFile    string // the hub's private key
	KnownHosts string // the bastion's host key
	RemotePort int    // the hub's port on the bastion's loopback
	LocalPort  int    // the hub's SSH server
}

// Result is what the file describes.
type Result struct {
	App       maintapp.Config
	Tunnel    Tunnel
	BrokerURL string
	ClientID  string
	StateDir  string
	LogLevel  slog.Level
}

type file struct {
	Site       string `json:"site"`
	BrokerURL  string `json:"broker_url"`
	ClientID   string `json:"client_id"`
	StateDir   string `json:"state_dir"`
	LogLevel   string `json:"log_level"`
	Enabled    *bool  `json:"enabled"`
	SSH        string `json:"ssh"`
	KeyFile    string `json:"key_file"`
	KnownHosts string `json:"known_hosts"`
	LocalPort  int    `json:"local_port"`
	MaxLength  string `json:"max_length"`
	Bastion    struct {
		Host       string `json:"host"`
		Port       int    `json:"port"`
		User       string `json:"user"`
		RemotePort int    `json:"remote_port"`
	} `json:"bastion"`
}

// Load reads the configuration at path.
func Load(path string) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("maintconfig: %w", err)
	}
	r, err := parse(data)
	if err != nil {
		return Result{}, fmt.Errorf("maintconfig: %s: %w", path, err)
	}
	return r, nil
}

func parse(data []byte) (Result, error) {
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
		return Result{}, errors.New("state_dir is required (where a session is kept across a restart)")
	}
	r := Result{BrokerURL: f.BrokerURL, ClientID: f.ClientID, StateDir: f.StateDir}
	if r.BrokerURL == "" {
		r.BrokerURL = "mqtt://127.0.0.1:1883"
	}
	if r.ClientID == "" {
		r.ClientID = "safehome-maint-" + f.Site
	}
	var err error
	if r.LogLevel, err = parseLevel(f.LogLevel); err != nil {
		return Result{}, err
	}
	m := maint.DefaultConfig()
	if f.Enabled != nil {
		m.Enabled = *f.Enabled
	}
	if f.MaxLength != "" {
		if m.MaxLength, err = time.ParseDuration(f.MaxLength); err != nil {
			return Result{}, fmt.Errorf("max_length %q: %w (write for example 30m or 1h)", f.MaxLength, err)
		}
	}
	r.App = maintapp.DefaultConfig(f.Site, m)
	if err := r.App.Validate(); err != nil {
		return Result{}, err
	}
	r.Tunnel = Tunnel{
		SSH: f.SSH, Host: f.Bastion.Host, Port: f.Bastion.Port, User: f.Bastion.User,
		KeyFile: f.KeyFile, KnownHosts: f.KnownHosts, RemotePort: f.Bastion.RemotePort, LocalPort: f.LocalPort,
	}
	if r.Tunnel.SSH == "" {
		r.Tunnel.SSH = "/usr/bin/ssh"
	}
	if r.Tunnel.Port == 0 {
		r.Tunnel.Port = 22
	}
	if r.Tunnel.LocalPort == 0 {
		r.Tunnel.LocalPort = 22
	}
	if !m.Enabled && f.Bastion.Host == "" {
		return r, nil // switched off: no bastion needed
	}
	t := r.Tunnel
	switch {
	case t.Host == "" || t.User == "":
		return Result{}, errors.New("bastion: host and user are required")
	case t.RemotePort < 1024 || t.RemotePort > 65535:
		return Result{}, errors.New("bastion: remote_port from 1024 to 65535, one per site (docs/maintenance.md)")
	case t.KeyFile == "" || t.KnownHosts == "":
		return Result{}, errors.New("key_file and known_hosts are required (the hub's key, the bastion's host key)")
	}
	return r, nil
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
