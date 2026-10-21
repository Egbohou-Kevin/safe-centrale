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
