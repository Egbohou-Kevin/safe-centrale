package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"safehome/internal/contract"
	"safehome/internal/install"
)

func (c *command) devices(ctx context.Context, s *install.Session) error {
	view, err := c.snapshot(ctx, s)
	if err != nil {
		return err
	}
	site, err := c.editor().Read()
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

func use(site install.Site, d contract.RegistryDevice) string {
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

func (c *command) watch(ctx context.Context, s *install.Session) error {
	site, err := c.editor().Read()
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
	site, err := c.editor().Read()
	if err != nil {
		return err
	}
	findings := install.Check(site, view.Devices)
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
