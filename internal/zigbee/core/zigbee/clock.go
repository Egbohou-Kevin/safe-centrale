package zigbee

import "time"

// pullBack brings back what a clock set back left in the future. The core follows
// the system clock, even when it steps backward (an RTC running fast that NTP
// corrects): standing still until the clock caught up would leave every device
// silent meanwhile, since Zigbee2MQTT's last_seen follows the clock too (see noNews).
// Within a run, every wait is moved back by the step, so that it keeps what was left of
// it: a pairing window stretched would say "pairing" over a network Zigbee2MQTT closed
// at the end of the piece it was asked for, and its next piece, asked late, would leave
// the network closed in between. Then, after a restart too, a deadline is never further
// than its whole wait from now, and what was heard is never later than now. The last
// words compared with a message's last_seen are not moved: noNews leaves aside those
// later than the message (moved to now, they would still be later than a message heard
// just before).
func (n *Network) pullBack(now time.Time) {
	if step := n.last.Sub(now); step > 0 {
		n.shift(-step)
	}
	n.last = now
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

// Stepped takes a forward step of the system clock, which the driver sees on the
// monotonic clock: a Pi without a battery clock starts from the last time it knew,
// hours late after a power cut, and NTP sets it forward once internet is there. A
// device not heard since the start is not silent for those hours (26 October 2026: a
// door "offline" from the start until it was opened). Every wait under way, and when
// each device last spoke, move with the clock, so that each keeps the time that really
// passed. The last news are not moved: they are what Zigbee2MQTT said, and its next
// message, dated on the new time, is later still.
func (n *Network) Stepped(d time.Duration) {
	if d > 0 {
		n.shift(d)
	}
}

// shift moves every wait under way by d, and when each device last spoke: the clock
// stepped by d.
func (n *Network) shift(d time.Duration) {
	move := func(t time.Time) time.Time {
		if t.IsZero() {
			return t
		}
		return t.Add(d)
	}
	n.s.PairUntil, n.s.PairRenew = move(n.s.PairUntil), move(n.s.PairRenew)
	n.s.PairAsk.Deadline, n.health.Deadline = move(n.s.PairAsk.Deadline), move(n.health.Deadline)
	for id, p := range n.s.Pending {
		p.Deadline = move(p.Deadline)
		n.s.Pending[id] = p
	}
	for ieee, j := range n.s.Joining {
		j.At = move(j.At)
		n.s.Joining[ieee] = j
	}
	for id, t := range n.seen {
		n.seen[id] = move(t)
	}
}
