package gsm

import (
	"slices"
	"time"
)

func (g *Gateway) outcome(o Outcome) []Action {
	g.lineAnswered(o)
	if !o.OK {
		return g.failed(o.Job, o.Line, o.Reason, o.Now)
	}
	return g.sent(o)
}

// lineAnswered frees the line that sent a job, unless it is busy with another job
// now: the answer came after the deadline and the line was given new work since.
func (g *Gateway) lineAnswered(o Outcome) {
	l, ok := g.s.Lines[o.Line]
	if !ok || l.Busy != busyJob {
		return
	}
	if j, ok := g.jobOn(o.Line); ok && j.ID != o.Job {
		return
	}
	l.free()
	l.NextCheck = o.Now.Add(g.cfg.CheckEvery) // the line just proved itself, or not
	g.s.Lines[o.Line] = l
}

// sent handles a job a line sent. A late success still counts: the SMS left.
func (g *Gateway) sent(o Outcome) []Action {
	i, ok := g.job(o.Job)
	if !ok {
		return nil
	}
	j := g.s.Jobs[i]
	g.remove(i)
	g.s.LastGood = o.Line
	if l, ok := g.s.Lines[o.Line]; ok {
		if l.Credit == CreditEmpty {
			l.Credit = CreditOK
		}
		l.Misses = 0 // the modem answers
		g.s.Lines[o.Line] = l
	}
	if j.Test {
		g.recordTest(o.Line, true, o.Now)
		return nil
	}
	g.told(j.Contact, OpenItem{Incident: j.Incident, At: o.Now, Priority: j.Priority})
	if j.Channel == ChannelSMS && o.MsgRef != "" {
		g.remember(o.Line+"/"+o.MsgRef, j.Ref)
	}
	return []Action{Report{Ref: j.Ref, Status: StatusSent}}
}

// failed handles a job a line could not send: try another line, or wait and try
// again. The first full round that fails is reported, so the escalation does not
// wait for the retries.
func (g *Gateway) failed(id uint64, line string, reason Reason, now time.Time) []Action {
	g.noteFailure(line, reason)
	i, ok := g.job(id)
	if !ok || g.s.Jobs[i].Line != line {
		return nil // a late answer: the job left, waits, or another line sends it now
	}
	j := &g.s.Jobs[i]
	j.Line, j.Deadline = "", time.Time{}
	if g.isClosed(j.Incident) {
		g.remove(i) // its incident ended while the line was sending it
		return nil
	}
	if !slices.Contains(j.Tried, line) {
		j.Tried = append(j.Tried, line)
	}
	j.Failure = reason
	if g.otherLineLeft(j.Tried) {
		return nil
	}
	return g.roundFailed(i, now)
}

// roundFailed ends the round of the job at index i, whose every usable line failed:
// it waits before the next round, and its first failed round is reported. A test is
// removed instead: it is not retried, its job is to tell.
func (g *Gateway) roundFailed(i int, now time.Time) []Action {
	j := &g.s.Jobs[i]
	if j.Test {
		if len(j.Tried) > 0 {
			g.recordTest(j.Tried[len(j.Tried)-1], false, now)
		}
		g.remove(i)
		return nil
	}
	j.Attempts++
	j.Tried = nil
	j.NotBefore = now.Add(g.backoff(j.Attempts))
	if j.Reported {
		return nil
	}
	j.Reported = true
	return []Action{Report{Ref: j.Ref, Status: StatusFailed, Reason: j.Failure}}
}

// closeRounds ends the round of each job that waits for a line it has not tried yet
// once no such line is usable any more (it lost the network, failed its check, was
// removed by a restart). Otherwise the job would wait until it expires, its failure
// never reported for the escalation to move on, even with the line it tried working
// again.
func (g *Gateway) closeRounds(now time.Time) []Action {
	var out []Action
	for i := len(g.s.Jobs) - 1; i >= 0; i-- { // from the end: a test may be removed
		j := g.s.Jobs[i]
		if j.Line == "" && len(j.Tried) > 0 && !g.otherLineLeft(j.Tried) {
			out = append(out, g.roundFailed(i, now)...)
		}
	}
	return out
}

// noteFailure keeps what a failure says about the line. A job that timed out or met
// a modem error is a miss, as a check without answer is (see restartStuck); any other
// failure is an answer from the modem.
func (g *Gateway) noteFailure(line string, reason Reason) {
	l, ok := g.s.Lines[line]
	if !ok {
		return
	}
	l.Misses = 0
	switch reason {
	case ReasonNoCredit:
		l.Credit = CreditEmpty
	case ReasonNoNetwork:
		l.Registered = false
	case ReasonTimeout, ReasonModemError:
		l.Misses = g.s.Lines[line].Misses + 1
	}
	g.s.Lines[line] = l
}

func (g *Gateway) recordTest(line string, ok bool, now time.Time) {
	l, known := g.s.Lines[line]
	if !known {
		return
	}
	l.LastTest, l.LastTestOK = now, &ok
	g.s.Lines[line] = l
}

func (g *Gateway) backoff(attempts int) time.Duration {
	d := g.cfg.RetryMin
	for i := 1; i < attempts && d < g.cfg.RetryMax; i++ {
		d *= 2
	}
	return min(d, g.cfg.RetryMax)
}

// remember keeps which request an SMS reference belongs to, for its delivery
// report. The oldest are forgotten.
func (g *Gateway) remember(key, ref string) {
	if _, ok := g.s.Pending[key]; !ok {
		g.s.Order = append(g.s.Order, key)
	}
	g.s.Pending[key] = ref
	for len(g.s.Order) > pendingSize {
		delete(g.s.Pending, g.s.Order[0])
		g.s.Order = g.s.Order[1:]
	}
}

func (g *Gateway) delivery(d DeliveryReport) []Action {
	key := d.Line + "/" + d.MsgRef
	ref, ok := g.s.Pending[key]
	if !ok {
		return nil
	}
	delete(g.s.Pending, key)
	g.s.Order = slices.DeleteFunc(g.s.Order, func(k string) bool { return k == key })
	if d.OK {
		return []Action{Report{Ref: ref, Status: StatusDelivered}}
	}
	return []Action{Report{Ref: ref, Status: StatusFailed, Reason: ReasonUnreachable}}
}
