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
	down := g.lineDown()
	g.s.Jobs = slices.DeleteFunc(g.s.Jobs, func(j Job) bool {
		drop := j.Incident == incident && j.Line == ""
		if drop && j.Ref != "" && !j.Reported {
			// Said, so that who reads the escalation never takes it for sent; never a second failure.
			out = append(out, Report{Ref: j.Ref, Status: StatusFailed, Reason: givenUp(j, down)})
		}
		return drop
	})
	if n := before - len(g.s.Jobs); n > 0 {
		return append(out, Dropped{Incident: incident, Jobs: n})
	}
	return nil
}

// confirmText tells a contact that the alarm was taken in hand in their name in the
// app.
const confirmText = "SeQ: l'alarme est prise en charge en votre nom dans l'application. Si ce n'est pas vous, personne ne s'en occupe: agissez."

// confirm tells by SMS the contact who acknowledged an incident in the app, once per
// incident: the acknowledgement comes through the cloud and ends the SMS and calls of
// the escalation; one the contact did not give must not end them unknown to them. A
// contact without a number cannot be told.
func (g *Gateway) confirm(c Cancel) {
	number, ok := g.numbers[c.Confirm]
	if c.Confirm == "" || c.Incident == "" || !ok || g.isClosed(c.Incident) {
		return
	}
	g.addJob(Job{Contact: c.Confirm, Number: number, Channel: ChannelSMS, Priority: PriorityIntrusion, Text: confirmText, Received: c.Now, Notice: true})
}

func (g *Gateway) isClosed(incident string) bool {
	return incident != "" && slices.Contains(g.s.Closed, incident)
}

// givenUp is the reason told for a job dropped because its incident ended: what kept
// it from leaving when that is known (a line that failed it, or every line out of
// service, down), dropped only when it merely waited its turn. Kevin's trial on a hub
// without a modem (25 October 2026): every message said "the alarm was over" when the
// line had been down all along.
func givenUp(j Job, down Reason) Reason {
	switch {
	case j.Failure != "":
		return j.Failure
	case down != "":
		return down
	}
	return ReasonDropped
}

// lineDown says why no line can send, empty when one can: no_network when a modem
// answers without the network, modem_error when none answers (or there is none).
func (g *Gateway) lineDown() Reason {
	why := ReasonModemError
	for _, id := range g.lineIDs() {
		l := g.s.Lines[id]
		if l.usable() {
			return ""
		}
		if l.Responding {
			why = ReasonNoNetwork
		}
	}
	return why
}
