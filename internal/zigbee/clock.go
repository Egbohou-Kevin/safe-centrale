package zigbee

import "time"

// pullBack brings back what a clock set back left in the future. The core follows
// the system clock, even when it steps backward (an RTC running fast that NTP
// corrects): standing still until the clock caught up would leave every device
// silent meanwhile, since Zigbee2MQTT's last_seen follows the clock too (see noNews).
// A deadline is never further than its whole wait from now, and what was heard is
// never later than now. The last words compared with a message's last_seen are not
// moved: noNews leaves aside those later than the message (moved to now, they would
// still be later than a message heard just before).
func (n *Network) pullBack(now time.Time) {
	clamp := func(t time.Time, d time.Duration) time.Time {
		if limit := now.Add(d); t.After(limit) {
			return limit
		}
		return t
	}
	n.s.PairUntil = clamp(n.s.PairUntil, n.cfg.PairMax)
	n.s.PairRenew = clamp(n.s.PairRenew, PermitJoinMax-renewBefore)
	n.s.PairAsk.Deadline = clamp(n.s.PairAsk.Deadline, n.cfg.ConfirmWait)
	n.health.Deadline = clamp(n.health.Deadline, n.cfg.ConfirmWait)
	for id, p := range n.s.Pending {
		p.Deadline = clamp(p.Deadline, n.cfg.ConfirmWait)
		n.s.Pending[id] = p
	}
	for ieee, j := range n.s.Joining {
		j.At = clamp(j.At, 0)
		n.s.Joining[ieee] = j
	}
	for id, t := range n.seen {
		n.seen[id] = clamp(t, 0)
	}
	for id, t := range n.news {
		n.news[id] = clamp(t, 0)
	}
}
