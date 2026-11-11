package escalation

import "time"

// Stepped takes a forward step d of the system clock, which the driver sees on the
// monotonic clock (driver.Core.Stepped): a Pi without a battery clock starts from the
// last time it knew, hours late after a power cut, and NTP sets it forward once
// internet is there. Every wait under way moves with the clock, so that each keeps the
// time that really passed: the next wave or reminder, the internet confirmation, the
// GSM takeover, and the windows of the hourly limit and of the day's memory. Otherwise
// every wave and reminder of an incident would go at once. When each incident was
// raised stays what it is: it is told (Send.RaisedAt), and its steps count from it plus
// Moved.
func (e *Escalation) Stepped(d time.Duration) {
	if d <= 0 {
		return
	}
	for key, in := range e.s.Incidents {
		in.Moved += d
		in.NextAt = moved(in.NextAt, d)
		for c, r := range in.Rounds {
			r.NetDeadline, r.GSMDue = moved(r.NetDeadline, d), moved(r.GSMDue, d)
			in.Rounds[c] = r
		}
		e.s.Incidents[key] = in
	}
	for _, sent := range []map[string][]time.Time{e.s.Recent, e.s.Lesser} {
		for _, ts := range sent {
			for i, t := range ts {
				ts[i] = moved(t, d)
			}
		}
	}
	for id, h := range e.s.Handled {
		h.At = moved(h.At, d)
		e.s.Handled[id] = h
	}
	for key, at := range e.s.SirenReported {
		e.s.SirenReported[key] = moved(at, d)
	}
}

// moved is t moved by d, the zero time staying zero (no deadline).
func moved(t time.Time, d time.Duration) time.Time {
	if t.IsZero() {
		return t
	}
	return t.Add(d)
}
