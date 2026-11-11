package zigbee

import (
	"strconv"
	"time"
)

// bridge follows Zigbee2MQTT's state. A pairing window ends when Zigbee2MQTT goes
// offline, and says so to whoever asked for it: Zigbee2MQTT starts again with the
// network closed, and the installer must not wait in front of a closed network.
//
// A retained "online" is no evidence: it is the broker's copy of what Zigbee2MQTT
// said when it last connected, perhaps before a power cut. One that left it unable
// to start (a stick that fails, damaged files) leaves that copy for good: Zigbee2MQTT
// opens the stick before it connects, never connects again, and so never says
// "offline", nor does its last will (the broker was down too). Zigbee2MQTT is asked
// whether it runs (a health check) and counts as online once it answers. A live
// "online" is its word as it connects.
func (n *Network) bridge(b Bridge) []Action {
	if !b.Online {
		n.s.Bridge, n.coded, n.health = false, false, Asking{}
		if !n.s.PairUntil.IsZero() {
			return n.failPairing(ReasonBridgeOffline)
		}
		return nil
	}
	if b.Retained && !n.s.Bridge {
		return n.askHealth(b.Now, 1)
	}
	return n.online()
}

// online records that Zigbee2MQTT runs. Each time it comes online, it is told again
// which devices carry a code: a keypad paired before, or a configuration of
// Zigbee2MQTT restored from a backup, gets its setting back.
func (n *Network) online() []Action {
	n.s.Bridge, n.health = true, Asking{}
	if n.coded {
		return nil
	}
	n.coded = true
	var out []Action
	for _, d := range n.Devices() {
		if n.byName[d.Profile].carriesCode() {
			out = append(out, CarriesCode{IEEE: d.IEEE})
		}
	}
	return out
}

// askHealth asks Zigbee2MQTT whether it runs. The transaction carries the time: the
// answer to a request of an earlier run is not taken for this one's.
func (n *Network) askHealth(now time.Time, attempt int) []Action {
	n.health = Asking{Txn: "health." + strconv.FormatInt(now.UnixMilli(), 10), Attempt: attempt, Deadline: now.Add(n.cfg.ConfirmWait)}
	return []Action{HealthCheck{Txn: n.health.Txn}}
}

// healthy reads Zigbee2MQTT's answer to the health check: whatever it says, it runs.
// A late answer counts too, once the requests are given up.
func (n *Network) healthy(h Health) []Action {
	if n.health.Txn == "" || h.Txn != n.health.Txn {
		return []Action{Ignored{What: "an answer to a health check that was not asked"}}
	}
	return n.online()
}

// healthDue asks again when Zigbee2MQTT did not answer, Config.Attempts times in all.
// Then the bridge stays offline until Zigbee2MQTT answers or says it is online, as it
// does each time it connects.
func (n *Network) healthDue(now time.Time) []Action {
	a := n.health
	if a.Deadline.IsZero() || now.Before(a.Deadline) {
		return nil
	}
	if a.Attempt >= n.cfg.Attempts {
		n.health.Deadline = time.Time{}
		return []Action{Ignored{What: "Zigbee2MQTT does not answer: its retained \"online\" is not believed"}}
	}
	return n.askHealth(now, a.Attempt+1)
}
