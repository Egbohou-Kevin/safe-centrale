package zigbee

import (
	"maps"
	"slices"
	"time"
)

// heard records that a device spoke, and brings it back online.
func (n *Network) heard(id string, now time.Time) []Action {
	n.seen[id] = now
	if !n.s.Offline[id] {
		return nil
	}
	delete(n.s.Offline, id)
	return []Action{Presence{DeviceID: id, Online: true, Since: now}}
}

func (n *Network) left(l Left) []Action {
	d, ok := n.byIEEE(l.IEEE)
	if !ok || n.s.Offline[d.ID] {
		return nil
	}
	n.s.Offline[d.ID] = true
	return []Action{Presence{DeviceID: d.ID, Since: l.Now}}
}

// markSilent takes offline the devices that have been silent for too long.
func (n *Network) markSilent(now time.Time) []Action {
	var out []Action
	for _, id := range slices.Sorted(maps.Keys(n.s.Devices)) {
		if n.s.Offline[id] || now.Before(n.seen[id].Add(n.offlineAfter(n.s.Devices[id]))) {
			continue
		}
		n.s.Offline[id] = true
		out = append(out, Presence{DeviceID: id, Since: now})
	}
	return out
}

func (n *Network) offlineAfter(d Device) time.Duration {
	if p, ok := n.byName[d.Profile]; ok && p.OfflineAfter > 0 {
		return p.OfflineAfter
	}
	return n.cfg.OfflineAfter
}
