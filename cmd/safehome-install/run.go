package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"safehome/internal/hubconfig"
	"safehome/internal/install"
)

const usage = `usage:
  safehome-install devices -config FILE                  paired devices and what each one is for
  safehome-install pair    -config FILE [-for 2m]        open the network and report the devices that join
  safehome-install remove  -config FILE DEVICE           remove a device for good (and its zone or siren role)
  safehome-install zone    -config FILE -name NAME -kind KIND DEVICE
                                                         give a device its zone
  safehome-install unzone  -config FILE DEVICE           take a device out of its zone
  safehome-install siren   -config FILE add|remove DEVICE
                                                         use a device as a siren, or stop
  safehome-install watch   -config FILE [DEVICE]         show device events live, for the tests
  safehome-install check   -config FILE                  compare the configuration with the paired devices
  safehome-install version

FILE is the hub's configuration (/etc/safehome/hub.json). Zones and sirens are kept
in the hub's state folder: run the tool as the hub's user (sudo -u safehome
safehome-install ...). After a change, restart the hub: sudo systemctl restart
safehome-hub. The local console does the same from a phone, in installation mode.
`

// Waits of the tool; the tests shorten them.
var (
	connectWait  = 5 * time.Second  // for the broker
	snapshotWait = 2 * time.Second  // for the retained registry
	timing       = install.Timing{} // the session's defaults
	pairMax      = 10 * time.Minute // the Zigbee adapter's own bound
	restartHint  = "restart the hub to apply: sudo systemctl restart safehome-hub"
)

// command is one parsed command line.
type command struct {
	cmd      string
	cfg      hubconfig.Result
	path     string
	args     []string // the positional arguments
	zoneName string   // -name
	kind     string   // -kind
	pairFor  time.Duration
	force    bool
	stdout   io.Writer
	stderr   io.Writer
}

// argCounts gives each command its least and most positional arguments.
var argCounts = map[string][2]int{
	"devices": {0, 0}, "pair": {0, 0}, "remove": {1, 1}, "zone": {1, 1},
	"unzone": {1, 1}, "siren": {2, 2}, "watch": {0, 1}, "check": {0, 0},
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c, exit, ok := parse(args, stdout, stderr)
	if !ok {
		return exit
	}
	var err error
	if c.cfg, err = hubconfig.Load(c.path); err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}
	if err := c.do(ctx); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

// parse reads the command line. When ok is false, the tool has nothing more to do
// and exits with exit: it printed its version, or what was wrong with the line.
func parse(args []string, stdout, stderr io.Writer) (c command, exit int, ok bool) {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return c, 2, false
	}
	c = command{cmd: args[0], stdout: stdout, stderr: stderr}
	if c.cmd == "version" {
		fmt.Fprintln(stdout, "safehome-install", version)
		return c, 0, false
	}
	bounds, known := argCounts[c.cmd]
	if !known {
		fmt.Fprintf(stderr, "unknown command %q\n%s", c.cmd, usage)
		return c, 2, false
	}
	rest, err := parseInterspersed(c.flags(stderr), args[1:])
	if err != nil {
		return c, 2, false
	}
	c.args = rest
	switch {
	case c.path == "":
		fmt.Fprintf(stderr, "%s: -config is required\n", c.cmd)
		return c, 2, false
	case len(c.args) < bounds[0] || len(c.args) > bounds[1]:
		fmt.Fprintf(stderr, "%s: wrong number of arguments\n%s", c.cmd, usage)
		return c, 2, false
	case c.cmd == "siren" && c.args[0] != "add" && c.args[0] != "remove":
		fmt.Fprintf(stderr, "siren: say add or remove, not %q\n", c.args[0])
		return c, 2, false
	}
	return c, 0, true
}

// flags declares the command's flags, filled into c.
func (c *command) flags(stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(c.cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.path, "config", "", "hub configuration file (required)")
	switch c.cmd {
	case "pair":
		fs.DurationVar(&c.pairFor, "for", 2*time.Minute, "how long the network stays open (10m at most)")
	case "zone":
		fs.StringVar(&c.zoneName, "name", "", "the zone's name, as people say it (required)")
		fs.StringVar(&c.kind, "kind", "", "the zone's kind: "+strings.Join(install.ZoneKinds(), ", ")+" (required)")
		fs.BoolVar(&c.force, "force", false, "accept a device that is not in the Zigbee registry (another adapter)")
	case "siren":
		fs.BoolVar(&c.force, "force", false, "accept a device that is not in the Zigbee registry (another adapter)")
	}
	return fs
}

// do runs the command. Those that only change the configuration file work without
// the broker.
func (c *command) do(ctx context.Context) error {
	switch {
	case c.cmd == "unzone":
		return c.unzone()
	case c.cmd == "siren" && c.args[0] == "remove":
		return c.sirenRemove()
	}
	return c.online(ctx, map[string]func(context.Context, *install.Session) error{
		"siren": c.sirenAdd, "devices": c.devices, "pair": c.pair, "remove": c.remove,
		"zone": c.zone, "watch": c.watch, "check": c.check,
	}[c.cmd])
}

// parseInterspersed parses flags placed before, between or after the positional
// arguments ("zone dev_k7p2 -name Entree" as well as "zone -name Entree dev_k7p2").
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}
