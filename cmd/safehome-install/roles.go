package main

import (
	"context"
	"fmt"

	"safehome/internal/contract"
	"safehome/internal/hubconfig"
	"safehome/internal/install"
)

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

// editor changes the zones and the sirens in the hub's state folder.
func (c *command) editor() *install.Editor {
	return install.NewEditor(hubconfig.SiteFiles{Path: c.path})
}

func (c *command) zone(ctx context.Context, s *install.Session) error {
	id, z := c.args[0], install.Zone{Name: c.zoneName, Kind: c.kind}
	if err := install.ValidZone(z); err != nil {
		return err
	}
	d, err := c.registered(ctx, s, id)
	if err != nil {
		return err
	}
	old, err := c.editor().SetZone(id, z, d)
	if err != nil {
		return err
	}
	if old != nil {
		fmt.Fprintf(c.stdout, "%s: zone %q (%s) is now %q (%s)\n", id, old.Name, old.Kind, z.Name, z.Kind)
	} else {
		fmt.Fprintf(c.stdout, "%s: zone %q (%s)\n", id, z.Name, z.Kind)
	}
	fmt.Fprintln(c.stdout, restartHint)
	return nil
}

func (c *command) unzone() error {
	id := c.args[0]
	z, err := c.editor().Unzone(id)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "%s: zone %q removed\n%s\n", id, z.Name, restartHint)
	return nil
}

// sirenAdd makes a device a siren, and names it when -name says so.
func (c *command) sirenAdd(ctx context.Context, s *install.Session) error {
	id := c.args[1]
	if c.zoneName != "" {
		if err := install.ValidSirenName(c.zoneName); err != nil {
			return err // before anything is written
		}
	}
	d, err := c.registered(ctx, s, id)
	if err != nil {
		return err
	}
	added, err := c.editor().AddSiren(id, d)
	switch {
	case err != nil:
		return err
	case !added:
		fmt.Fprintf(c.stdout, "%s is already a siren\n", id)
	default:
		fmt.Fprintf(c.stdout, "%s is now a siren\n", id)
	}
	if c.zoneName != "" {
		return c.sirenName()
	}
	if added {
		fmt.Fprintln(c.stdout, restartHint)
	}
	return nil
}

// sirenName gives a siren its name for the alerts' texts.
func (c *command) sirenName() error {
	id := c.args[1]
	old, err := c.editor().NameSiren(id, c.zoneName)
	switch {
	case err != nil:
		return err
	case old == c.zoneName:
		fmt.Fprintf(c.stdout, "%s: siren already named %q\n", id, old)
		return nil
	case old != "":
		fmt.Fprintf(c.stdout, "%s: siren %q is now %q\n", id, old, c.zoneName)
	default:
		fmt.Fprintf(c.stdout, "%s: siren named %q\n", id, c.zoneName)
	}
	fmt.Fprintln(c.stdout, restartHint)
	return nil
}

func (c *command) sirenRemove() error {
	id := c.args[1]
	removed, err := c.editor().RemoveSiren(id)
	switch {
	case err != nil:
		return err
	case !removed:
		return fmt.Errorf("%s is not a siren", id)
	}
	fmt.Fprintf(c.stdout, "%s is no longer a siren\n%s\n", id, restartHint)
	return nil
}
