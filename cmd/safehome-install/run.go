package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/contract"
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

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	c := command{cmd: args[0], stdout: stdout, stderr: stderr}
	positional := map[string][2]int{ // min and max positional arguments
		"devices": {0, 0}, "pair": {0, 0}, "remove": {1, 1}, "zone": {1, 1},
		"unzone": {1, 1}, "siren": {2, 2}, "watch": {0, 1}, "check": {0, 0},
	}
	if c.cmd == "version" {
		fmt.Fprintln(stdout, "safehome-install", version)
		return 0
	}
	bounds, ok := positional[c.cmd]
	if !ok {
		fmt.Fprintf(stderr, "unknown command %q\n%s", c.cmd, usage)
		return 2
	}

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
	rest, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return 2
	}
	c.args = rest
	if c.path == "" {
		fmt.Fprintf(stderr, "%s: -config is required\n", c.cmd)
		return 2
	}
	if len(c.args) < bounds[0] || len(c.args) > bounds[1] {
		fmt.Fprintf(stderr, "%s: wrong number of arguments\n%s", c.cmd, usage)
		return 2
	}
	if c.cfg, err = hubconfig.Load(c.path); err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}

	switch c.cmd {
	case "unzone":
		err = c.unzone()
	case "siren":
		if c.args[0] != "add" && c.args[0] != "remove" {
			fmt.Fprintf(stderr, "siren: say add or remove, not %q\n", c.args[0])
			return 2
		}
		if c.args[0] == "remove" {
			err = c.sirenRemove()
			break
		}
		err = c.online(ctx, c.sirenAdd)
	case "devices":
		err = c.online(ctx, c.devices)
	case "pair":
		err = c.online(ctx, c.pair)
	case "remove":
		err = c.online(ctx, c.remove)
	case "zone":
		err = c.online(ctx, c.zone)
	case "watch":
		err = c.online(ctx, c.watch)
	case "check":
		err = c.online(ctx, c.check)
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
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

// --- connection ------------------------------------------------------------------

// brokerPublisher lets the session publish through the MQTT client.
type brokerPublisher struct{ c *mqtt.Client }

func (b brokerPublisher) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	return b.c.Publish(ctx, mqtt.Message{Topic: topic, Payload: payload, QoS: qos, Retain: retain})
}

// online connects to the broker, runs fn with a session, and disconnects.
func (c *command) online(ctx context.Context, fn func(context.Context, *install.Session) error) error {
	ids := contract.NewIDGenerator()
	pub := &brokerPublisher{}
	s, err := install.NewSession(c.cfg.Hub.Site, install.Deps{Publisher: pub, Now: time.Now, NewID: ids.New}, timing)
	if err != nil {
		return err
	}
	var subs []mqtt.Subscription
	for _, sub := range s.Subscriptions() {
		subs = append(subs, mqtt.Subscription{Filter: sub.Filter, QoS: sub.QoS})
	}
	connCtx, stopConn := context.WithCancel(context.Background()) // outlives ctx: Ctrl-C still closes the network
	defer stopConn()
	client, err := mqtt.Dial(connCtx, mqtt.Options{
		BrokerURL: c.cfg.BrokerURL, ClientID: clientID(c.cfg.Hub.Site), Ephemeral: true, Subscriptions: subs,
		OnMessage: func(m mqtt.Message) { s.Deliver(install.Message{Topic: m.Topic, Payload: m.Payload, Retain: m.Retain}) },
	})
	if err != nil {
		return err
	}
	pub.c = client
	defer func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		client.Close(closeCtx)
		stop()
	}()
	awaitCtx, stopAwait := context.WithTimeout(ctx, connectWait)
	defer stopAwait()
	if err := client.Await(awaitCtx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("cannot reach the broker at %s: is mosquitto running?", c.cfg.BrokerURL)
	}
	return fn(ctx, s)
}

// clientID is unique for each run: two tools may run at once (watch in one terminal,
// pair in another) without pushing each other off the broker.
func clientID(site string) string {
	var b [4]byte
	rand.Read(b[:])
	return "safehome-install-" + site + "-" + hex.EncodeToString(b[:])
}

func (c *command) snapshot(ctx context.Context, s *install.Session) (install.View, error) {
	snapCtx, stop := context.WithTimeout(ctx, snapshotWait)
	defer stop()
	return s.Snapshot(snapCtx)
}

// --- the commands ----------------------------------------------------------------

func (c *command) devices(ctx context.Context, s *install.Session) error {
	view, err := c.snapshot(ctx, s)
	if err != nil {
		return err
	}
	site, err := hubconfig.ReadSite(c.path)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, statusLine(view.Status))
	if len(view.Devices) == 0 {
		fmt.Fprintln(c.stdout, "no device paired yet: safehome-install pair -config", c.path)
		return nil
	}
	devices := slices.Clone(view.Devices)
	slices.SortFunc(devices, func(a, b contract.RegistryDevice) int { return strings.Compare(a.DeviceID, b.DeviceID) })
	w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "DEVICE\tMODEL\tCAPABILITIES\tUSE")
	for _, d := range devices {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.DeviceID, d.Model, strings.Join(d.Capabilities, ", "), use(site, d))
	}
	w.Flush()
	if view.Status != nil && len(view.Status.Offline) > 0 {
		fmt.Fprintln(c.stdout, "offline (not heard for a long time):", strings.Join(view.Status.Offline, ", "))
	}
	return nil
}

func statusLine(st *contract.ZigbeeStatusData) string {
	if st == nil {
		return "Zigbee adapter: no status published yet"
	}
	pairing := "closed"
	if st.Pairing {
		pairing = "open"
	}
	return fmt.Sprintf("Zigbee2MQTT: %s, %d device(s), pairing %s", st.Bridge, st.Devices, pairing)
}

func use(site hubconfig.Site, d contract.RegistryDevice) string {
	var uses []string
	if z, ok := site.Zones[d.DeviceID]; ok {
		uses = append(uses, fmt.Sprintf("zone %q (%s)", z.Name, z.Kind))
	}
	if slices.Contains(site.SirenIDs, d.DeviceID) {
		uses = append(uses, "siren")
	}
	if slices.Contains(d.Capabilities, contract.CapKeypad) {
		uses = append(uses, "keypad")
	}
	if len(uses) == 0 {
		return "- (unused)"
	}
	return strings.Join(uses, ", ")
}

// rejectReasons explains the adapter's refusals to the installer.
var rejectReasons = map[string]string{
	contract.RejectUnknownModel:    "no profile for this model, it cannot be watched",
	contract.RejectNotPairing:      "it joined while the network was closed",
	contract.RejectInterviewFailed: "its model could not be read, try again closer to the hub",
}

func (c *command) pair(ctx context.Context, s *install.Session) error {
	if c.pairFor <= 0 || c.pairFor > pairMax {
		return fmt.Errorf("-for must be between 1s and %v", pairMax)
	}
	fmt.Fprintf(c.stdout, "opening the network for %v: activate each device now (Ctrl-C to stop)\n", c.pairFor)
	added := 0
	err := s.Pair(ctx, c.pairFor, func(n install.PairNews) {
		switch {
		case n.Added != nil:
			added++
			fmt.Fprintf(c.stdout, "+ %s  %s  (%s)\n    next: safehome-install zone -config %s -name NAME -kind KIND %s\n",
				n.Added.DeviceID, n.Added.Model, strings.Join(n.Added.Capabilities, ", "), c.path, n.Added.DeviceID)
		case n.Rejected != nil:
			model := n.Rejected.Model
			if model == "" {
				model = "unknown model"
			}
			why := rejectReasons[n.Rejected.Reason]
			if why == "" {
				why = n.Rejected.Reason
			}
			fmt.Fprintf(c.stdout, "x refused %s: %s\n", model, why)
		}
	})
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		fmt.Fprintf(c.stdout, "stopped: network closed, %d device(s) added\n", added)
		return nil
	}
	fmt.Fprintf(c.stdout, "network closed: %d device(s) added\n", added)
	return nil
}

func (c *command) remove(ctx context.Context, s *install.Session) error {
	id := c.args[0]
	site, err := hubconfig.ReadSite(c.path)
	if err != nil {
		return err
	}
	z, zoned := site.Zones[id]
	siren := slices.Contains(site.SirenIDs, id)
	delete(site.Zones, id)
	site.SirenIDs = slices.DeleteFunc(site.SirenIDs, func(s string) bool { return s == id })
	// A removal cannot be undone (the device would have to be paired again, with a new
	// id): make sure the configuration can follow before doing it.
	if zoned || siren {
		if err := hubconfig.CheckSite(c.path, site); err != nil {
			return fmt.Errorf("%s cannot be removed yet, the configuration could not follow (add its replacement first): %w", id, err)
		}
	}
	if err := s.Remove(ctx, id); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "%s removed from the network for good\n", id)
	if !zoned && !siren {
		return nil
	}
	if err := hubconfig.WriteSite(c.path, site); err != nil {
		return fmt.Errorf("the device is removed but the configuration still uses it: %w", err)
	}
	if zoned {
		fmt.Fprintf(c.stdout, "zone %q removed from the configuration\n", z.Name)
	}
	if siren {
		fmt.Fprintln(c.stdout, "siren removed from the configuration")
	}
	fmt.Fprintln(c.stdout, restartHint)
	return nil
}

// registered finds a device in the registry. With -force, a device the registry does
// not know (one of another adapter) is accepted unchecked.
func (c *command) registered(ctx context.Context, s *install.Session, id string) (*contract.RegistryDevice, error) {
	view, err := c.snapshot(ctx, s)
	if err != nil && !c.force {
		return nil, fmt.Errorf("%w (with -force, the device is accepted unchecked)", err)
	}
	for _, d := range view.Devices {
		if d.DeviceID == id {
			return &d, nil
		}
	}
	if c.force {
		fmt.Fprintf(c.stdout, "warning: %s is not in the Zigbee registry, accepted unchecked (-force)\n", id)
		return nil, nil
	}
	return nil, fmt.Errorf("%s is not paired (see: safehome-install devices); a device of another adapter needs -force", id)
}

func (c *command) zone(ctx context.Context, s *install.Session) error {
	id := c.args[0]
	if err := install.ValidName(c.zoneName); err != nil {
		return err
	}
	if err := install.ValidKind(c.kind); err != nil {
		return err
	}
	d, err := c.registered(ctx, s, id)
	if err != nil {
		return err
	}
	if d != nil {
		if err := install.CanServe(c.kind, d.Capabilities); err != nil {
			return fmt.Errorf("%s cannot serve as this zone: %w", id, err)
		}
	}
	site, err := hubconfig.ReadSite(c.path)
	if err != nil {
		return err
	}
	old, had := site.Zones[id]
	site.Zones[id] = hubconfig.Zone{Name: c.zoneName, Kind: c.kind}
	if err := hubconfig.WriteSite(c.path, site); err != nil {
		return err
	}
	if had {
		fmt.Fprintf(c.stdout, "%s: zone %q (%s) is now %q (%s)\n", id, old.Name, old.Kind, c.zoneName, c.kind)
	} else {
		fmt.Fprintf(c.stdout, "%s: zone %q (%s)\n", id, c.zoneName, c.kind)
	}
	fmt.Fprintln(c.stdout, restartHint)
	return nil
}

func (c *command) unzone() error {
	id := c.args[0]
	site, err := hubconfig.ReadSite(c.path)
	if err != nil {
		return err
	}
	z, ok := site.Zones[id]
	if !ok {
		return fmt.Errorf("%s has no zone", id)
	}
	delete(site.Zones, id)
	if err := hubconfig.WriteSite(c.path, site); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "%s: zone %q removed\n%s\n", id, z.Name, restartHint)
	return nil
}

func (c *command) sirenAdd(ctx context.Context, s *install.Session) error {
	id := c.args[1]
	d, err := c.registered(ctx, s, id)
	if err != nil {
		return err
	}
	if d != nil && !slices.Contains(d.Capabilities, contract.CapSiren) {
		return fmt.Errorf("%s cannot sound (it has %s)", id, strings.Join(d.Capabilities, ", "))
	}
	site, err := hubconfig.ReadSite(c.path)
	if err != nil {
		return err
	}
	if slices.Contains(site.SirenIDs, id) {
		fmt.Fprintf(c.stdout, "%s is already a siren\n", id)
		return nil
	}
	site.SirenIDs = append(site.SirenIDs, id)
	if err := hubconfig.WriteSite(c.path, site); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "%s is now a siren\n%s\n", id, restartHint)
	return nil
}

func (c *command) sirenRemove() error {
	id := c.args[1]
	site, err := hubconfig.ReadSite(c.path)
	if err != nil {
		return err
	}
	if !slices.Contains(site.SirenIDs, id) {
		return fmt.Errorf("%s is not a siren", id)
	}
	site.SirenIDs = slices.DeleteFunc(site.SirenIDs, func(s string) bool { return s == id })
	if err := hubconfig.WriteSite(c.path, site); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "%s is no longer a siren\n%s\n", id, restartHint)
	return nil
}

func (c *command) watch(ctx context.Context, s *install.Session) error {
	site, err := hubconfig.ReadSite(c.path)
	if err != nil {
		return err
	}
	only := ""
	if len(c.args) == 1 {
		only = c.args[0]
	}
	loc := c.cfg.Hub.Engine.Location
	if loc == nil {
		loc = time.Local
	}
	fmt.Fprintln(c.stdout, "watching device events: trigger each device now (Ctrl-C to stop)")
	err = s.Watch(ctx, only, func(at time.Time, e contract.DeviceEventData) {
		name := "-"
		if z, ok := site.Zones[e.DeviceID]; ok {
			name = z.Name
		}
		fmt.Fprintf(c.stdout, "%s  %-12s %-16s %s = %s\n", at.In(loc).Format("15:04:05"), e.DeviceID, name, e.Capability, e.Value)
	})
	if n := s.Dropped(); n > 0 {
		fmt.Fprintf(c.stderr, "warning: %d message(s) dropped, the terminal was too slow\n", n)
	}
	return err
}

func (c *command) check(ctx context.Context, s *install.Session) error {
	view, err := c.snapshot(ctx, s)
	if err != nil {
		return err
	}
	site, err := hubconfig.ReadSite(c.path)
	if err != nil {
		return err
	}
	zones := map[string]install.Zone{}
	for id, z := range site.Zones {
		zones[id] = install.Zone{Name: z.Name, Kind: z.Kind}
	}
	findings := install.Check(install.Site{Zones: zones, SirenIDs: site.SirenIDs}, view.Devices)
	for _, f := range findings {
		fmt.Fprintln(c.stdout, f)
	}
	if install.HasProblem(findings) {
		return errors.New("the configuration has problems to fix before handing over")
	}
	fmt.Fprintf(c.stdout, "OK: %d zone(s), %d siren(s), %d paired device(s)", len(site.Zones), len(site.SirenIDs), len(view.Devices))
	if len(findings) > 0 {
		fmt.Fprintf(c.stdout, ", %d warning(s)", len(findings))
	}
	fmt.Fprintln(c.stdout)
	return nil
}
