// Package consoleconfig reads the local console's configuration file (JSON),
// strictly: a misspelt key is an error. See config/console.example.json and
// docs/security-install.md.
package consoleconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"safehome/internal/consoleapp"
	"safehome/internal/contract"
)

// Wifi is the hub's own network, which exists while the installation mode lasts.
type Wifi struct {
	Interface  string
	SSID       string
	Passphrase string // read from passphrase_file; never printed
	Address    string // the hub's address on it, where the page is
}

// LogValue keeps the passphrase out of the logs.
func (w Wifi) LogValue() slog.Value {
	return slog.GroupValue(slog.String("interface", w.Interface), slog.String("ssid", w.SSID), slog.String("address", w.Address))
}

// Result is what the file describes.
type Result struct {
	App       consoleapp.Config
	Wifi      Wifi
	Port      int
	HubConfig string // the hub's configuration: its zones and sirens, its state folder
	BrokerURL string
	ClientID  string
	StateDir  string
	LogLevel  slog.Level
}

// Listen is where the page is served: the hub's address on its own network.
func (r Result) Listen() string { return net.JoinHostPort(r.Wifi.Address, strconv.Itoa(r.Port)) }

// Host is what the phone's browser sends as Host: the address, and the port unless 80.
func (r Result) Host() string {
	if r.Port == 80 {
		return r.Wifi.Address
	}
	return r.Listen()
}

type file struct {
	Site      string `json:"site"`
	BrokerURL string `json:"broker_url"`
	ClientID  string `json:"client_id"`
	StateDir  string `json:"state_dir"`
	LogLevel  string `json:"log_level"`
	HubConfig string `json:"hub_config"`
	Port      int    `json:"port"`
	PairFor   string `json:"pair_for"`
	Wifi      *struct {
		Interface      string `json:"interface"`
		SSID           string `json:"ssid"`
		PassphraseFile string `json:"passphrase_file"`
		Address        string `json:"address"`
	} `json:"wifi"`
}

// Load reads the configuration at path. A relative passphrase_file is taken from the
// configuration file's folder.
func Load(path string) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("consoleconfig: %w", err)
	}
	r, err := parse(data, filepath.Dir(path))
	if err != nil {
		return Result{}, fmt.Errorf("consoleconfig: %s: %w", path, err)
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
		return Result{}, errors.New("state_dir is required (the hub's, where the zones and sirens are kept)")
	}
	if f.HubConfig == "" {
		return Result{}, errors.New("hub_config is required (the hub's configuration file)")
	}
	r := Result{BrokerURL: f.BrokerURL, ClientID: f.ClientID, StateDir: f.StateDir, HubConfig: f.HubConfig, Port: f.Port}
	if r.BrokerURL == "" {
		r.BrokerURL = "mqtt://127.0.0.1:1883"
	}
	if r.ClientID == "" {
		r.ClientID = "safehome-console-" + f.Site
	}
	if r.Port == 0 {
		r.Port = 80
	}
	if r.Port < 1 || r.Port > 65535 {
		return Result{}, fmt.Errorf("port %d: from 1 to 65535", r.Port)
	}
	var err error
	if r.LogLevel, err = parseLevel(f.LogLevel); err != nil {
		return Result{}, err
	}
	r.App = consoleapp.DefaultConfig(f.Site)
	if f.PairFor != "" {
		if r.App.PairFor, err = time.ParseDuration(f.PairFor); err != nil || r.App.PairFor <= 0 || r.App.PairFor > 10*time.Minute {
			return Result{}, fmt.Errorf("pair_for %q: from 1s to 10m (the Zigbee adapter's own bound), for example 2m", f.PairFor)
		}
	}
	if !contract.ValidSite(f.Site) {
		return Result{}, fmt.Errorf("invalid site %q", f.Site)
	}

	w := f.Wifi
	if w == nil {
		return Result{}, errors.New("wifi is required (the hub's own network for the console)")
	}
	r.Wifi = Wifi{Interface: w.Interface, SSID: w.SSID, Address: w.Address}
	if r.Wifi.Interface == "" {
		r.Wifi.Interface = "wlan0"
	}
	if r.Wifi.SSID == "" {
		r.Wifi.SSID = "safehome-" + f.Site
	}
	if r.Wifi.Address == "" {
		r.Wifi.Address = "10.42.0.1"
	}
	if ip := net.ParseIP(r.Wifi.Address); ip == nil || ip.To4() == nil {
		return Result{}, fmt.Errorf("wifi.address %q: an IPv4 address", r.Wifi.Address)
	}
	if w.PassphraseFile == "" {
		return Result{}, errors.New("wifi.passphrase_file is required (the passphrase printed on the hub's label)")
	}
	pf := w.PassphraseFile
	if !filepath.IsAbs(pf) {
		pf = filepath.Join(dir, pf)
	}
	b, err := os.ReadFile(pf)
	if err != nil {
		return Result{}, fmt.Errorf("wifi.passphrase_file: %w", err)
	}
	r.Wifi.Passphrase = strings.TrimSpace(string(b))
	if n := utf8.RuneCountInString(r.Wifi.Passphrase); n < 8 || len(r.Wifi.Passphrase) > 63 {
		return Result{}, errors.New("wifi.passphrase_file: the passphrase must be 8 to 63 characters (WPA2)")
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
