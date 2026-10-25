package gsm

import (
	"cmp"
	"slices"
	"time"
)

const testText = "safeHome: SMS de test, ne pas repondre."

func (g *Gateway) request(r Request) []Action {
	if r.Channel != ChannelSMS && r.Channel != ChannelCall {
		return nil
	}
	if slices.Contains(g.s.Seen, r.ID) {
		return nil // delivered twice by the broker: one SMS only
	}
	g.s.Seen = append(g.s.Seen, r.ID)
	if len(g.s.Seen) > seenSize {
		g.s.Seen = g.s.Seen[len(g.s.Seen)-seenSize:]
	}
	if g.isClosed(r.Incident) {
		return []Action{Dropped{Incident: r.Incident, Jobs: 1}} // delivered after its incident ended
	}
	asked := askedAt(r)
	if !r.Now.Before(asked.Add(g.cfg.MaxAge[r.Priority])) {
		return []Action{Report{Ref: r.ID, Status: StatusFailed, Reason: ReasonTimeout}}
	}
	number, ok := g.numbers[r.Contact]
	if !ok {
		return []Action{Report{Ref: r.ID, Status: StatusFailed, Reason: ReasonUnreachable}}
	}
	g.addJob(Job{
		Ref: r.ID, Incident: r.Incident, Contact: r.Contact, Number: number,
		Channel: r.Channel, Priority: r.Priority, Text: r.Text, Received: asked,
	})
	return []Action{Report{Ref: r.ID, Status: StatusQueued}}
}

// askedAt is when a request was asked, which its age counts from. A time ahead of
// its arrival can only come from a clock set back since (the hub and the gateway
// read the same clock): it counts from the arrival, so the request is not kept for
// longer than its maximum age.
func askedAt(r Request) time.Time {
	if r.Asked.IsZero() || r.Asked.After(r.Now) {
		return r.Now
	}
	return r.Asked
}

func (g *Gateway) addJob(j Job) {
	g.s.NextJob++
	j.ID = g.s.NextJob
	g.s.Jobs = append(g.s.Jobs, j)
}

func (g *Gateway) job(id uint64) (int, bool) {
	i := slices.IndexFunc(g.s.Jobs, func(j Job) bool { return j.ID == id })
	return i, i >= 0
}

// jobOn returns the job a line is sending.
func (g *Gateway) jobOn(line string) (Job, bool) {
	i := slices.IndexFunc(g.s.Jobs, func(j Job) bool { return j.Line == line })
	if i < 0 {
		return Job{}, false
	}
	return g.s.Jobs[i], true
}

func (g *Gateway) remove(i int) { g.s.Jobs = slices.Delete(g.s.Jobs, i, i+1) }

// --- time -----------------------------------------------------------------------------

// timers handles what time alone decides: the lines that did not answer in time,
// the requests too old to send and the periodic test SMS.
func (g *Gateway) timers(now time.Time) []Action {
	out := g.lineTimeouts(now)
	out = append(out, g.dropExpired(now)...)
	g.queueTest(now)
	return out
}

// lineTimeouts ends what a line did not finish in time: a job it did not answer
// for failed, a check without answer counts as silence, a restart is over.
func (g *Gateway) lineTimeouts(now time.Time) []Action {
	var out []Action
	for _, id := range g.lineIDs() {
		l := g.s.Lines[id]
		if l.Busy == "" || now.Before(l.BusyUntil) {
			continue
		}
		switch l.Busy {
		case busyJob:
			if j, ok := g.jobOn(id); ok {
				out = append(out, g.failed(j.ID, id, ReasonTimeout, now)...)
			}
			l = g.s.Lines[id]
			l.free()
			g.s.Lines[id] = l
		case busyCheck:
			out = append(out, g.probe(Probe{Now: now, Line: id})...)
		case busyRestart:
			g.restarted(id)
		}
	}
	return out
}

// dropExpired gives up the requests too old to send: the next wave of the
// escalation took over. A job in flight waits for its line's answer.
func (g *Gateway) dropExpired(now time.Time) []Action {
	var out []Action
	kept := g.s.Jobs[:0]
	for _, j := range g.s.Jobs {
		if j.Line != "" || now.Before(j.Received.Add(g.cfg.MaxAge[j.Priority])) {
			kept = append(kept, j)
			continue
		}
		if !j.Test && !j.Reported {
			out = append(out, Report{Ref: j.Ref, Status: StatusFailed, Reason: ReasonTimeout})
		}
	}
	g.s.Jobs = kept
	return out
}

// queueTest queues the test SMS when it is due. The first one goes a full period
// after the gateway first started.
func (g *Gateway) queueTest(now time.Time) {
	if g.test == "" || (!g.s.NextTest.IsZero() && now.Before(g.s.NextTest)) {
		return
	}
	first := g.s.NextTest.IsZero()
	g.s.NextTest = now.Add(g.cfg.TestEvery)
	if first {
		return
	}
	g.addJob(Job{Number: g.test, Channel: ChannelSMS, Priority: PriorityOther, Text: testText, Received: now, Test: true})
}

// --- scheduling -----------------------------------------------------------------------

// schedule gives work to the free lines: the most urgent jobs first (an alert must
// not wait behind a routine check), then the checks that are due on the lines left
// idle.
func (g *Gateway) schedule(now time.Time) []Action {
	out := g.closeRounds(now)
	out = append(out, g.startJobs(now)...)
	return append(out, g.startChecks(now)...)
}

func (g *Gateway) startJobs(now time.Time) []Action {
	var out []Action
	for _, i := range g.byUrgency() {
		j := &g.s.Jobs[i]
		if j.Line != "" || j.NotBefore.After(now) {
			continue
		}
		j.NotBefore = time.Time{} // its wait is over, even if no line is free yet
		if line, ok := g.pickLine(j.Tried); ok {
			out = append(out, g.start(j, line, now))
		}
	}
	return out
}

// byUrgency returns the indexes of the jobs, the most urgent first, then the
// oldest.
func (g *Gateway) byUrgency() []int {
	order := make([]int, len(g.s.Jobs))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		ja, jb := g.s.Jobs[a], g.s.Jobs[b]
		return cmp.Or(cmp.Compare(ja.Priority, jb.Priority), ja.Received.Compare(jb.Received), cmp.Compare(ja.ID, jb.ID))
	})
	return order
}

// start hands a job to a line, which has until its deadline to answer.
func (g *Gateway) start(j *Job, line string, now time.Time) Execute {
	ex := Execute{Job: j.ID, Line: line, Channel: j.Channel, Number: j.Number, Text: j.Text}
	timeout := g.cfg.SMSTimeout
	if j.Channel == ChannelCall {
		ex.RingFor = g.cfg.RingFor
		timeout += g.cfg.RingFor
	}
	l := g.s.Lines[line]
	l.Busy, l.BusyUntil = busyJob, now.Add(timeout)
	g.s.Lines[line] = l
	j.Line, j.Deadline = line, l.BusyUntil
	return ex
}

func (g *Gateway) startChecks(now time.Time) []Action {
	var out []Action
	for _, id := range g.lineIDs() {
		l := g.s.Lines[id]
		if l.Busy != "" || l.NextCheck.After(now) {
			continue
		}
		l.Busy, l.BusyUntil = busyCheck, now.Add(g.cfg.CheckTimeout)
		g.s.Lines[id] = l
		out = append(out, CheckLine{Line: id})
	}
	return out
}

// pickLine returns a free usable line not yet tried in this round: the one that
// sent last first, then in configuration order.
func (g *Gateway) pickLine(tried []string) (string, bool) {
	candidates := g.lineIDs()
	if g.s.LastGood != "" {
		candidates = append([]string{g.s.LastGood}, candidates...)
	}
	for _, id := range candidates {
		l, ok := g.s.Lines[id]
		if ok && l.Busy == "" && l.usable() && !slices.Contains(tried, id) {
			return id, true
		}
	}
	return "", false
}

// otherLineLeft reports whether a usable line remains untried in this round.
func (g *Gateway) otherLineLeft(tried []string) bool {
	for _, id := range g.lineIDs() {
		if g.s.Lines[id].usable() && !slices.Contains(tried, id) {
			return true
		}
	}
	return false
}
