// Package consoleconfig reads the local console's configuration file (JSON),
// strictly: a misspelt key is an error. See config/console.example.json and
// docs/security-install.md.
package consoleconfig

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"time"

	"safehome/internal/configfile"
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

// String hides the passphrase, for %v and %+v (of a Result too).
func (w Wifi) String() string {
	return fmt.Sprintf("{interface:%s ssid:%q passphrase:*** address:%s}", w.Interface, w.SSID, w.Address)
}

// GoString hides the passphrase for %#v.
func (w Wifi) GoString() string { return w.String() }

// LogValue keeps the passphrase out of the logs.
func (w Wifi) LogValue() slog.Value {
	return slog.GroupValue(slog.String("interface", w.Interface), slog.String("ssid", w.SSID), slog.String("address", w.Address))
}

// Result is what the file describes.
type Result struct {
	configfile.Program
	App       consoleapp.Config
	Wifi      Wifi
	Port      int
	HubConfig string // the hub's configuration: its zones and sirens, its state folder
}

// Listen is where the page is served: the hub's address on its own network.
func (r Result) Listen() string { return net.JoinHostPort(r.Wifi.Address, strconv.Itoa(r.Port)) }

// Clients is the hub's own network, the /24 of its address (see hotspot): the only
// addresses the page answers.
func (r Result) Clients() netip.Prefix {
	addr, _ := netip.ParseAddr(r.Wifi.Address) // checked at load: an IPv4 address
	p, _ := addr.Prefix(24)
	return p
}

// Host is what the phone's browser sends as Host: the address, and the port unless 80.
func (r Result) Host() string {
	if r.Port == 80 {
		return r.Wifi.Address
	}
	return r.Listen()
}

// wifiFile is the "wifi" object of the file.
type wifiFile struct {
	Interface      string `json:"interface"`
	SSID           string `json:"ssid"`
	PassphraseFile string `json:"passphrase_file"`
	Address        string `json:"address"`
}

type file struct {
	configfile.ProgramFile
	HubConfig string    `json:"hub_config"`
	Port      int       `json:"port"`
	PairFor   string    `json:"pair_for"`
	Wifi      *wifiFile `json:"wifi"`
}

// Load reads the configuration at path. A relative passphrase_file is taken from the
// configuration file's folder.
func Load(path string) (Result, error) {
	var f file
	if err := configfile.Decode(path, &f); err != nil {
		return Result{}, fmt.Errorf("consoleconfig: %w", err)
	}
	r, err := build(f, filepath.Dir(path))
	if err != nil {
		return Result{}, fmt.Errorf("consoleconfig: %s: %w", path, err)
	}
	return r, nil
}

func build(f file, dir string) (Result, error) {
	r, err := parseService(f)
	if err != nil {
		return Result{}, err
	}
	if f.Wifi == nil {
		return Result{}, errors.New("wifi is required (the hub's own network for the console)")
	}
	if r.Wifi, err = parseWifi(*f.Wifi, f.Site, dir); err != nil {
		return Result{}, err
	}
	return r, nil
}

// parseService reads everything but the network, with its defaults.
func parseService(f file) (Result, error) {
	if !contract.ValidSite(f.Site) {
		return Result{}, fmt.Errorf("invalid site %q", f.Site)
	}
	p, err := f.Build("safehome-console-", "the hub's, where the zones and sirens are kept")
	if err != nil {
		return Result{}, err
	}
	if f.HubConfig == "" {
		return Result{}, errors.New("hub_config is required (the hub's configuration file)")
	}
	r := Result{Program: p, HubConfig: f.HubConfig, Port: cmp.Or(f.Port, 80), App: consoleapp.DefaultConfig(f.Site)}
	if r.Port < 1 || r.Port > 65535 {
		return Result{}, fmt.Errorf("port %d: from 1 to 65535", r.Port)
	}
	if f.PairFor != "" {
		if r.App.PairFor, err = time.ParseDuration(f.PairFor); err != nil || r.App.PairFor <= 0 || r.App.PairFor > 10*time.Minute {
			return Result{}, fmt.Errorf("pair_for %q: from 1s to 10m (the Zigbee adapter's own bound), for example 2m", f.PairFor)
		}
	}
	return r, nil
}

// parseWifi reads the network, with its defaults, and its passphrase from its own
// file (relative to dir).
func parseWifi(w wifiFile, site, dir string) (Wifi, error) {
	out := Wifi{
		Interface: cmp.Or(w.Interface, "wlan0"),
		SSID:      cmp.Or(w.SSID, "safehome-"+site),
		Address:   cmp.Or(w.Address, "10.42.0.1"),
	}
	if ip := net.ParseIP(out.Address); ip == nil || ip.To4() == nil {
		return Wifi{}, fmt.Errorf("wifi.address %q: an IPv4 address", out.Address)
	}
	if w.PassphraseFile == "" {
		return Wifi{}, errors.New("wifi.passphrase_file is required (the passphrase printed on the hub's label)")
	}
	var err error
	if out.Passphrase, err = configfile.Secret(dir, w.PassphraseFile); err != nil {
		return Wifi{}, fmt.Errorf("wifi.passphrase_file: %w", err)
	}
	if !validPassphrase(out.Passphrase) {
		return Wifi{}, errors.New("wifi.passphrase_file: the passphrase must be 8 to 63 printable ASCII characters (WPA2)")
	}
	return out, nil
}

// validPassphrase reports whether s is a WPA2 passphrase: 8 to 63 printable ASCII
// characters. The network would refuse anything else, but only when it starts, in
// the middle of an installation.
func validPassphrase(s string) bool {
	if len(s) < 8 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}
