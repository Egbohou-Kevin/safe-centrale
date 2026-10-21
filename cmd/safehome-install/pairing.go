package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"safehome/internal/contract"
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
			fmt.Fprintf(c.stdout, "+ %s  %s  (%s)\n    next: %s\n", n.Added.DeviceID, n.Added.Model,
				strings.Join(n.Added.Capabilities, ", "), c.nextStep(n.Added.DeviceID, n.Added.Capabilities))
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

// nextStep is the command that gives a new device its use: a siren is made one, the
// rest get a zone.
func (c *command) nextStep(id string, caps []string) string {
	if slices.Contains(caps, contract.CapSiren) {
		return fmt.Sprintf("safehome-install siren -config %s add %s", c.path, id)
	}
	return fmt.Sprintf("safehome-install zone -config %s -name NAME -kind KIND %s", c.path, id)
}

func (c *command) remove(ctx context.Context, s *install.Session) error {
	id, editor := c.args[0], c.editor()
	if _, err := editor.CheckRemoval(id); err != nil {
		return err
	}
	if err := s.Remove(ctx, id); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "%s removed from the network for good\n", id)
	u, err := editor.FinishRemoval(id)
	if err != nil || !u.Used() {
		return err
	}
	if u.Zone != nil {
		fmt.Fprintf(c.stdout, "zone %q removed from the configuration\n", u.Zone.Name)
	}
	if u.Siren {
		fmt.Fprintln(c.stdout, "siren removed from the configuration")
	}
	fmt.Fprintln(c.stdout, restartHint)
	return nil
}
