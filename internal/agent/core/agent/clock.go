package agent

import "time"

// Stepped takes a forward step d of the system clock, which the driver sees on the
// monotonic clock (driver.Core.Stepped): a Pi without a battery clock starts from the
// last time it knew, hours late after a power cut, and NTP sets it forward once
// internet is there. Every wait under way moves with the clock, so that each keeps the
// time that really passed: an alert waiting for the cloud's confirmation does not fail
// at once (which would send the escalation to the GSM for nothing), and what the relay
// remembers for a while (the alerts, the incidents, the tombstones, the photos, the
// allowances, the states said again, the repeated messages, a move's deadline) is not
// forgotten early.
// Each item of the queue keeps its age too, in memory: its copy on disk is not written
// again for it (a restart starts from the clock as it is, and the queue keeps items a
// week). The times set by the cloud (a photo's URL expiry) are not moved.
func (r *Relay) Stepped(d time.Duration) {
	if d <= 0 {
		return
	}
	for ref, p := range r.s.Pending {
		p.Deadline, p.Forget = moved(p.Deadline, d), moved(p.Forget, d)
		r.s.Pending[ref] = p
	}
	for _, m := range []map[string]time.Time{r.s.GivenUp, r.s.Incidents, r.restated} {
		for k, at := range m {
			m[k] = moved(at, d)
		}
	}
	r.s.TokensAt, r.s.AnswersAt, r.s.RepliesAt = moved(r.s.TokensAt, d), moved(r.s.AnswersAt, d), moved(r.s.RepliesAt, d)
	for id, p := range r.s.Photos {
		p.Taken, p.RetryAt = moved(p.Taken, d), moved(p.RetryAt, d)
		r.s.Photos[id] = p
	}
	if r.s.Move != nil {
		r.s.Move.GiveUp = moved(r.s.Move.GiveUp, d)
	}
	for i := range r.queue {
		r.queue[i].At = moved(r.queue[i].At, d)
	}
	for i := range r.replay {
		r.replay[i].At = moved(r.replay[i].At, d)
	}
	for i := range r.tombs {
		r.tombs[i].at = moved(r.tombs[i].at, d)
	}
	for k, m := range r.got {
		m.at = moved(m.at, d)
		r.got[k] = m
	}
}

// moved is t moved by d, the zero time staying zero (no deadline).
func moved(t time.Time, d time.Duration) time.Time {
	if t.IsZero() {
		return t
	}
	return t.Add(d)
}
