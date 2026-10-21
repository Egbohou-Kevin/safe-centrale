package main

import (
	"safehome/internal/hubconfig"
	"safehome/internal/install"
)

// sites fits the hub's files (hubconfig) to the console's Sites port.
type sites struct{ hubConfig string }

func (s sites) Read() (install.Site, error) {
	h, err := hubconfig.ReadSite(s.hubConfig)
	if err != nil {
		return install.Site{}, err
	}
	out := install.Site{Zones: map[string]install.Zone{}, SirenIDs: h.SirenIDs}
	for id, z := range h.Zones {
		out.Zones[id] = install.Zone{Name: z.Name, Kind: z.Kind}
	}
	return out, nil
}

func toHub(s install.Site) hubconfig.Site {
	out := hubconfig.Site{Zones: map[string]hubconfig.Zone{}, SirenIDs: s.SirenIDs}
	for id, z := range s.Zones {
		out.Zones[id] = hubconfig.Zone{Name: z.Name, Kind: z.Kind}
	}
	return out
}

func (s sites) Check(site install.Site) error { return hubconfig.CheckSite(s.hubConfig, toHub(site)) }
func (s sites) Write(site install.Site) error { return hubconfig.WriteSite(s.hubConfig, toHub(site)) }
