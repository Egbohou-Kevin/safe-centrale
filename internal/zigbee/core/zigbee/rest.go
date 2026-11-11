package zigbee

import (
	"maps"
	"slices"
	"time"
)

// restDue brings back to rest the states that have a rest value (Rule.Rest) and stayed
// away from it for RestAfter with no message from their device. Zigbee2MQTT says "no
// movement" on a timer it keeps in memory, about 90 s after the movement: stopped
// within that time (a crash, an update, a power cut), it loses the timer, and the cache
// it replays when it starts still says "detected". Nothing would ever say "no
// movement" again: the next movement would change nothing, and the stale "detected"
// would set off the alarm at the hub's next restart while armed. A movement that goes
// on keeps the device talking (a message every minute or so): its state stays.
func (n *Network) restDue(now time.Time) []Action {
	var out []Action
	for _, id := range slices.Sorted(maps.Keys(n.s.States)) {
		due, away := n.restAt(id)
		if len(away) == 0 || now.Before(due) {
			continue
		}
		rules := n.byName[n.s.Devices[id].Profile].Capabilities
		for _, c := range away {
			if ch, moved := n.setState(id, c, rules[c].Rest, rules[c], Report{Now: now}); moved {
				out = append(out, ch)
			}
		}
	}
	return out
}

// restAt returns the capabilities of a device away from their rest value, sorted, and
// when they go back to it unless the device speaks first.
func (n *Network) restAt(id string) (time.Time, []string) {
	d, ok := n.s.Devices[id]
	if !ok {
		return time.Time{}, nil
	}
	var away []string
	for c, rule := range n.byName[d.Profile].Capabilities {
		if st, ok := n.s.States[id][c]; ok && rule.Rest != "" && st.Value != rule.Rest {
			away = append(away, c)
		}
	}
	slices.Sort(away)
	return n.seen[id].Add(n.cfg.RestAfter), away
}
