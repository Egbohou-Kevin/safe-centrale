// Package hotspot turns the hub's own Wi-Fi network on and off for the local console,
// through NetworkManager's command-line client (nmcli), which Raspberry Pi OS uses.
// The network exists only while the installation mode lasts: a connection made for
// it, never started at boot, deleted when it ends.
//
// The phone of the installer joins it with the passphrase printed on the hub's label
// and finds the console at the hub's address on it. NetworkManager gives the
// addresses (its "shared" mode). Standard library only.
package hotspot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// Config says which network to make.
type Config struct {
	Interface  string // the Wi-Fi interface, "wlan0"
	SSID       string // the network's name, 1 to 32 bytes
	Passphrase string // WPA2, 8 to 63 printable ASCII characters: on the hub's label
	Address    string // the hub's address on it, "10.42.0.1" (a /24)
	Connection string // NetworkManager's name for it, "safehome-console"
	NMCLI      string // the client's path, "nmcli" by default
}

// Hotspot turns the network on and off.
type Hotspot struct{ cfg Config }

// New checks the configuration.
func New(cfg Config) (*Hotspot, error) {
	if cfg.NMCLI == "" {
		cfg.NMCLI = "nmcli"
	}
	if cfg.Connection == "" {
		cfg.Connection = "safehome-console"
	}
	ip := net.ParseIP(cfg.Address)
	switch {
	case cfg.Interface == "":
		return nil, errors.New("hotspot: the Wi-Fi interface is required")
	case len(cfg.SSID) < 1 || len(cfg.SSID) > 32:
		return nil, errors.New("hotspot: the network name must be 1 to 32 bytes")
	case !validPassphrase(cfg.Passphrase):
		return nil, errors.New("hotspot: the passphrase must be 8 to 63 printable ASCII characters (WPA2)")
	case ip == nil || ip.To4() == nil:
		return nil, fmt.Errorf("hotspot: %q is not an IPv4 address", cfg.Address)
	}
	return &Hotspot{cfg: cfg}, nil
}

// validPassphrase reports whether s is a WPA2 passphrase: 8 to 63 printable ASCII
// characters. NetworkManager refuses anything else, but only when the network
// starts, in the middle of an installation.
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

// Up makes the network and starts it. A connection left by a crash is replaced.
func (h *Hotspot) Up(ctx context.Context) error {
	c := h.cfg
	h.run(ctx, "connection", "delete", c.Connection) // nothing to delete, most of the time
	if err := h.run(ctx, "connection", "add", "type", "wifi", "ifname", c.Interface, "con-name", c.Connection,
		"autoconnect", "no", "ssid", c.SSID,
		"802-11-wireless.mode", "ap", "802-11-wireless.band", "bg",
		"ipv4.method", "shared", "ipv4.addresses", c.Address+"/24", "ipv6.method", "disabled",
		"wifi-sec.key-mgmt", "wpa-psk", "wifi-sec.proto", "rsn", "wifi-sec.pairwise", "ccmp", "wifi-sec.psk", c.Passphrase,
	); err != nil {
		return err
	}
	if err := h.run(ctx, "connection", "up", c.Connection); err != nil {
		cleanCtx, cancel := detached(ctx)
		defer cancel()
		h.run(cleanCtx, "connection", "delete", c.Connection)
		return err
	}
	return nil
}

// Down stops the network and deletes it. It runs even when ctx is cancelled: the
// network must not outlive the installation mode. It keeps ctx's deadline, though: a
// hung nmcli must not hold up the next mode or the console's stop.
func (h *Hotspot) Down(ctx context.Context) error {
	ctx, cancel := detached(ctx)
	defer cancel()
	h.run(ctx, "connection", "down", h.cfg.Connection) // already down: nothing to say
	return h.run(ctx, "connection", "delete", h.cfg.Connection)
}

// cleanupWait bounds a cleanup whose caller gave no deadline.
const cleanupWait = 30 * time.Second

// detached returns a context that ignores ctx's cancellation but keeps its deadline,
// or cleanupWait when it has none.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(cleanupWait)
	}
	return context.WithDeadline(context.WithoutCancel(ctx), deadline)
}

// run runs nmcli. Its error carries what nmcli said, never the passphrase.
func (h *Hotspot) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, h.cfg.NMCLI, args...)
	cmd.WaitDelay = time.Second // a child left holding the output must not hold the call
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		said := strings.TrimSpace(strings.ReplaceAll(out.String(), h.cfg.Passphrase, "***"))
		return fmt.Errorf("hotspot: nmcli %s: %v: %s", args[0]+" "+args[1], err, said)
	}
	return nil
}
