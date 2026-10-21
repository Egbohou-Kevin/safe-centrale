// Package maintconfig reads the remote maintenance service's configuration file
// (JSON), strictly: a misspelt key is an error. See config/maint.example.json and
// docs/maintenance.md.
package maintconfig

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"

	"safehome/internal/configfile"
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
	var f file
	if err := configfile.Decode(path, &f); err != nil {
		return Result{}, fmt.Errorf("maintconfig: %w", err)
	}
	r, err := build(f)
	if err != nil {
		return Result{}, fmt.Errorf("maintconfig: %s: %w", path, err)
	}
	return r, nil
}

func build(f file) (Result, error) {
	if f.StateDir == "" {
		return Result{}, errors.New("state_dir is required (where a session is kept across a restart)")
	}
	r := Result{
		BrokerURL: cmp.Or(f.BrokerURL, "mqtt://127.0.0.1:1883"),
		ClientID:  cmp.Or(f.ClientID, "safehome-maint-"+f.Site),
		StateDir:  f.StateDir,
		Tunnel:    tunnel(f),
	}
	var err error
	if r.LogLevel, err = configfile.Level(f.LogLevel); err != nil {
		return Result{}, err
	}
	m := maint.DefaultConfig()
	if f.Enabled != nil {
		m.Enabled = *f.Enabled
	}
	if err := configfile.Duration("max_length", f.MaxLength, &m.MaxLength); err != nil {
		return Result{}, err
	}
	r.App = maintapp.DefaultConfig(f.Site, m)
	if err := r.App.Validate(); err != nil {
		return Result{}, err
	}
	if !m.Enabled && f.Bastion.Host == "" {
		return r, nil // switched off: no bastion needed
	}
	if err := checkTunnel(r.Tunnel); err != nil {
		return Result{}, err
	}
	return r, nil
}

// tunnel reads how to reach the bastion, with the defaults.
func tunnel(f file) Tunnel {
	return Tunnel{
		SSH: cmp.Or(f.SSH, "/usr/bin/ssh"), Host: f.Bastion.Host, Port: cmp.Or(f.Bastion.Port, 22), User: f.Bastion.User,
		KeyFile: f.KeyFile, KnownHosts: f.KnownHosts, RemotePort: f.Bastion.RemotePort, LocalPort: cmp.Or(f.LocalPort, 22),
	}
}

func checkTunnel(t Tunnel) error {
	switch {
	case t.Host == "" || t.User == "":
		return errors.New("bastion: host and user are required")
	case t.RemotePort < 1024 || t.RemotePort > 65535:
		return errors.New("bastion: remote_port from 1024 to 65535, one per site (docs/maintenance.md)")
	case t.KeyFile == "" || t.KnownHosts == "":
		return errors.New("key_file and known_hosts are required (the hub's key, the bastion's host key)")
	}
	return nil
}
