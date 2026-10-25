package zigbee

import (
	"maps"
	"slices"
	"time"
)

// heard records that a device spoke, and brings it back online: now is when its
// message reached us, news when Zigbee2MQTT heard it (see spokeAt).
func (n *Network) heard(id string, now, news time.Time) []Action {
	n.spokeAt(id, now, news)
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
	return []Action{Presence{DeviceID: d.ID, Since: l.Now, Heard: n.lastHeard(d.ID)}}
}

// markSilent takes offline the devices that have been silent for too long.
func (n *Network) markSilent(now time.Time) []Action {
	var out []Action
	for _, id := range slices.Sorted(maps.Keys(n.s.Devices)) {
		if n.s.Offline[id] || now.Before(n.seen[id].Add(n.offlineAfter(n.s.Devices[id]))) {
			continue
		}
		n.s.Offline[id] = true
		out = append(out, Presence{DeviceID: id, Since: now, Heard: n.lastHeard(id)})
	}
	return out
}

// spokeAt records that a device spoke. Its silence counts from now, when the message
// reached us; its last news is news, when Zigbee2MQTT heard it. The two differ for a
// message the broker kept while the adapter was down: its arrival is no news of the
// device, and dating the last news from it would tell the owner the door was watched
// longer than it was. The last news never goes back.
func (n *Network) spokeAt(id string, now, news time.Time) {
	n.seen[id] = now
	if last, ok := n.news[id]; !ok || news.After(last) {
		n.news[id] = news
	}
}

// lastHeard is a device's last news, to the minute (the owner needs no more), or zero
// if it did not speak since the adapter started: the start is no news from it.
func (n *Network) lastHeard(id string) time.Time {
	news, ok := n.news[id]
	if !ok {
		return time.Time{}
	}
	return news.Truncate(time.Minute)
}

func (n *Network) offlineAfter(d Device) time.Duration {
	if p, ok := n.byName[d.Profile]; ok && p.OfflineAfter > 0 {
		return p.OfflineAfter
	}
	return n.cfg.OfflineAfter
}
