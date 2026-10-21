package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"safehome/internal/contract"
	"safehome/internal/hubconfig"
	"safehome/internal/install"
)

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
