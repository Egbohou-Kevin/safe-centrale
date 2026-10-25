package gsm

import "slices"

// cancel ends an incident: its waiting messages are dropped, and the incident is
// remembered so that one a line is sending is not retried and a request of it
// delivered late is not sent.
func (g *Gateway) cancel(incident string) []Action {
	if incident == "" || g.isClosed(incident) {
		return nil // a test SMS has no incident
	}
	g.s.Closed = append(g.s.Closed, incident)
	if len(g.s.Closed) > closedSize {
		g.s.Closed = g.s.Closed[len(g.s.Closed)-closedSize:]
	}
	var out []Action
	before := len(g.s.Jobs)
	g.s.Jobs = slices.DeleteFunc(g.s.Jobs, func(j Job) bool {
		drop := j.Incident == incident && j.Line == ""
		if drop && j.Ref != "" && !j.Reported {
			// Said, so that who reads the escalation never takes it for sent; never a second failure.
			out = append(out, Report{Ref: j.Ref, Status: StatusFailed, Reason: ReasonDropped})
		}
		return drop
	})
	if n := before - len(g.s.Jobs); n > 0 {
		return append(out, Dropped{Incident: incident, Jobs: n})
	}
	return nil
}

func (g *Gateway) isClosed(incident string) bool {
	return incident != "" && slices.Contains(g.s.Closed, incident)
}
