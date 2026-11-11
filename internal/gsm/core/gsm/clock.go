package gsm

import "time"

// Stepped takes a forward step d of the system clock, which the driver sees on the
// monotonic clock (driver.Core.Stepped): a Pi without a battery clock starts from the
// last time it knew, hours late after a power cut, and NTP sets it forward once
// internet is there. Every wait under way moves with the clock, so that each keeps the
// time that really passed: the age of each request in the queue (an alert waiting for
// the network is not given up as too old), its wait before the next round, the time
// given to a line, the next check and the next test SMS, and how long ago each contact
// was told (for a "1"). When an incident was first told is not moved: it is compared
// with the operator's stamp on a "1", not with the time that passed. The times only
// said (a line's last test) are not moved either.
func (g *Gateway) Stepped(d time.Duration) {
	if d <= 0 {
		return
	}
	for i := range g.s.Jobs {
		j := &g.s.Jobs[i]
		j.Received, j.NotBefore, j.Deadline = moved(j.Received, d), moved(j.NotBefore, d), moved(j.Deadline, d)
	}
	for id, l := range g.s.Lines {
		l.BusyUntil, l.NextCheck = moved(l.BusyUntil, d), moved(l.NextCheck, d)
		g.s.Lines[id] = l
	}
	g.s.NextTest = moved(g.s.NextTest, d)
	for c, o := range g.s.Open {
		g.s.Open[c] = o.stepped(d)
	}
	for c, told := range g.s.Told {
		for i, o := range told {
			told[i] = o.stepped(d)
		}
		g.s.Told[c] = told
	}
}

// stepped is the item told d later on the clock read, still first told when it was.
func (o OpenItem) stepped(d time.Duration) OpenItem {
	o.First = o.first()
	o.At = moved(o.At, d)
	return o
}

// moved is t moved by d, the zero time staying zero (no deadline).
func moved(t time.Time, d time.Duration) time.Time {
	if t.IsZero() {
		return t
	}
	return t.Add(d)
}
