package zigbee

import (
	"maps"
	"strconv"
	"strings"
	"time"
)

// JoinWait is how long a device that entered during a pairing window has for
// Zigbee2MQTT to finish questioning it. The questioning starts when the device enters
// and may end well after the window (an Aqara door answers only while its button is
// tapped, about 30 s): the device is decided by the window it entered, not by the one
// open when its questioning ends.
const JoinWait = 3 * time.Minute

// joined records a device entering during a pairing window, under the window's order.
// One entering outside a window is not: interviewed refuses it.
func (n *Network) joined(j Joined) []Action {
	if !n.pairing(j.Now) {
		return nil
	}
	if _, known := n.byIEEE(j.IEEE); known {
		return nil // a registered device joining again
	}
	if n.s.Joining == nil {
		n.s.Joining = map[string]Joining{}
	}
	n.s.Joining[j.IEEE] = Joining{At: j.Now, Ref: n.s.PairRef}
	return nil
}

// pairing tells whether a pairing window is running at now.
func (n *Network) pairing(now time.Time) bool {
	return !n.s.PairUntil.IsZero() && now.Before(n.s.PairUntil)
}

// windowOf says under which pairing order a device whose questioning ended is
// decided: the window it entered, or the one running now. False: it entered outside
// any window.
func (n *Network) windowOf(ieee string, now time.Time) (string, bool) {
	j, ok := n.s.Joining[ieee]
	n.forgetJoining(func(id string, _ Joining) bool { return id == ieee })
	switch {
	case ok && now.Before(j.At.Add(JoinWait)):
		return j.Ref, true
	case n.pairing(now):
		return n.s.PairRef, true
	}
	return "", false
}

// joinsDue forgets the devices whose questioning did not end in time: Zigbee2MQTT gave
// up on them, or its word was lost. One that speaks afterwards is a stranger.
func (n *Network) joinsDue(now time.Time) {
	n.forgetJoining(func(_ string, j Joining) bool { return !now.Before(j.At.Add(JoinWait)) })
}

func (n *Network) forgetJoining(drop func(string, Joining) bool) {
	maps.DeleteFunc(n.s.Joining, drop)
	if len(n.s.Joining) == 0 {
		n.s.Joining = nil
	}
}

// stranger answers a message from a device outside the registry that is still in
// Zigbee2MQTT's network, where nobody watches it: one removed while asleep (it did not
// answer the request to leave), one refused whose expulsion failed the same way, or
// one that entered while the adapter was down (it only stops cleanly after closing the
// network) and whose questioning it never heard. It is asked to leave now that it is
// awake, then removed by force if it speaks again; once each per run. A device still
// named by its IEEE address during a pairing window, or being questioned, is left
// alone: it is not decided yet. Any other name is not a device of ours.
func (n *Network) stranger(r Report) []Action {
	name := r.Name
	removed := n.s.Retired[name]
	switch {
	case r.Retained || !(removed || isIEEE(name)):
		return []Action{Ignored{What: "a message from a device that is not registered"}}
	case !removed && n.undecided(name, r.Now):
		return []Action{Ignored{What: "a message from a device being questioned"}}
	case n.expelled[name] >= 2:
		return []Action{Ignored{What: "a message from a device already asked to leave"}}
	}
	forced := n.expelled[name] == 1
	n.expelled[name]++
	out := []Action{Expel{IEEE: name, Force: forced}, Stray{Name: name, Removed: removed, Forced: forced}}
	if !removed && !forced {
		out = append(out, Rejected{Reason: RejectNotPairing})
	}
	return out
}

func (n *Network) undecided(ieee string, now time.Time) bool {
	j, ok := n.s.Joining[ieee]
	return n.pairing(now) || (ok && now.Before(j.At.Add(JoinWait)))
}

// isIEEE tells whether a name is an IEEE address as Zigbee2MQTT writes it: the name
// it gives a device that was not renamed.
func isIEEE(name string) bool {
	hex, ok := strings.CutPrefix(name, "0x")
	if !ok || len(hex) != 16 {
		return false
	}
	_, err := strconv.ParseUint(hex, 16, 64)
	return err == nil
}
