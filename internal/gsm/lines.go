package gsm

import (
	"slices"
	"strings"
	"time"
)

// probe takes the answer to a line check. A check without answer is a miss (see
// restartStuck).
func (g *Gateway) probe(p Probe) []Action {
	l, ok := g.s.Lines[p.Line]
	if !ok {
		return nil
	}
	if l.Busy == busyCheck {
		l.free()
	}
	l.NextCheck = p.Now.Add(g.cfg.CheckEvery)
	if p.Responding {
		l.Responding, l.Registered, l.Signal, l.Misses = true, p.Registered, roundSignal(p.Signal), 0
		if p.Operator != "" {
			l.Operator = p.Operator
		}
	} else {
		l.Responding, l.Registered = false, false
		l.Misses++
	}
	g.s.Lines[p.Line] = l
	return nil
}

// restartStuck restarts the modem of each idle line that missed MissesToRestart times
// in a row: checks without answer and jobs that timed out or met a modem error alike.
// A hung modem fails every job, and while alerts keep its line busy no check comes to
// count its silence: counted on checks alone, it was restarted only once the queue
// drained, after the alarm.
func (g *Gateway) restartStuck(now time.Time) []Action {
	var out []Action
	for _, id := range g.lineIDs() {
		l := g.s.Lines[id]
		if l.Busy != "" || l.Misses < g.cfg.MissesToRestart {
			continue
		}
		l.Responding, l.Registered, l.Misses = false, false, 0
		l.Restarts++
		l.Busy, l.BusyUntil = busyRestart, now.Add(g.cfg.RestartTimeout)
		g.s.Lines[id] = l
		out = append(out, RestartModem{Line: id})
	}
	return out
}

// roundSignal keeps the signal to the nearest ten, from 0 to 100.
func roundSignal(v int) int { return (min(max(v, 0), 100) + 5) / 10 * 10 }

// restarted ends a modem restart; the line is checked again at once.
func (g *Gateway) restarted(line string) {
	l, ok := g.s.Lines[line]
	if !ok || l.Busy != busyRestart {
		return
	}
	l.free()
	l.NextCheck = time.Time{}
	g.s.Lines[line] = l
}

// ackAmbiguity is how close together two incidents told to a contact make their "1"
// ambiguous (see ackTarget). The texts all end the same way ("Repondre 1 si vous
// gerez"): nothing says which a "1" answers.
const ackAmbiguity = 10 * time.Minute

// told records an incident sent to a contact: the last one, and those sent to them
// within ackAmbiguity before it. An incident told again (a reminder) is as recent as
// its last message: told an hour ago and reminded a minute ago, it is still one a "1"
// may be meant for. It keeps when it was first told, which breaks ties (see
// ackTarget), and the order of the list.
func (g *Gateway) told(contact string, item OpenItem) {
	g.s.Open[contact] = item
	item.First = item.At
	cutoff := item.At.Add(-ackAmbiguity)
	var kept []OpenItem
	for _, o := range g.s.Told[contact] {
		if o.Incident == item.Incident {
			item.First, item.Priority = o.first(), min(o.Priority, item.Priority)
			continue
		}
		if !o.At.Before(cutoff) {
			kept = append(kept, o)
		}
	}
	kept = append(kept, item)
	slices.SortStableFunc(kept, func(a, b OpenItem) int { return a.first().Compare(b.first()) })
	g.s.Told[contact] = kept
}

// ackTarget is the incident a "1" acknowledges, after open, the last one told to
// contact: the one that ends least (see lesser) among those still open that were told
// to the contact within ackAmbiguity before it, a reminder included, the first told of
// them if several, open itself if none. Two alerts close together (a tamper, then the
// intrusion two minutes later) do not say which the "1" answers: ending the more
// serious one for everyone because of a reply meant for the lesser would stop its
// waves and reminders. Ending the lesser fails safe: the more serious one keeps going,
// and a second "1" ends it.
func (g *Gateway) ackTarget(contact string, open OpenItem) OpenItem {
	target, found := open, false
	for _, o := range g.s.Told[contact] {
		if g.isClosed(o.Incident) || o.At.Before(open.At.Add(-ackAmbiguity)) {
			continue
		}
		if !found || lesser(o, target) {
			target, found = o, true
		}
	}
	return target
}

// lesser tells whether acknowledging a ends less than acknowledging b: it is less
// urgent, or as urgent and secondary to an alarm told alongside it (a silent siren,
// whose acknowledgement ends only it, where the intrusion's ends its waves for all).
func lesser(a, b OpenItem) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.Secondary && !b.Secondary
}

// sentSlack is how far the operator's service centre's clock, which stamps an SMS, may
// be behind the hub's: a "1" stamped more than that before the alert it would answer
// was written before that alert existed.
const sentSlack = 10 * time.Minute

// incoming turns a contact's "1" into the acknowledgement of the incident they
// were last told about, within AckWindow, or of a less urgent one told just before
// (see ackTarget). A "1" written before that incident was first told (held by the
// network while the hub's SIM was off the air, stored while the gateway was down) is
// read after it went out, but answers something else: it ends nothing, the safe side.
func (g *Gateway) incoming(in Incoming) []Action {
	ignored := func(why string) []Action { return []Action{Ignored{Line: in.Line, Why: why}} }
	number, err := NormalizeNumber(in.From, g.cfg.Country)
	contacts := g.byPhone[number]
	if err != nil || len(contacts) == 0 {
		return ignored("unknown number")
	}
	if strings.TrimSpace(in.Text) != "1" {
		return ignored("unknown text")
	}
	contact, open, ok := g.lastTold(contacts)
	if !ok || in.Now.Sub(open.At) > g.cfg.AckWindow {
		return ignored("nothing to acknowledge")
	}
	open = g.ackTarget(contact, open)
	if !in.Sent.IsZero() && in.Sent.Add(sentSlack).Before(open.first()) {
		return ignored("written before the alert it would answer")
	}
	// The broker brings the acknowledgement back as a Cancel too; dropping now
	// spares the round trip.
	return append([]Action{Acknowledge{Incident: open.Incident, Contact: contact}}, g.cancel(open.Incident)...)
}

// lastTold returns, among contacts who share a phone, the one told most recently.
func (g *Gateway) lastTold(contacts []string) (string, OpenItem, bool) {
	var (
		best  string
		item  OpenItem
		found bool
	)
	for _, c := range contacts {
		if o, ok := g.s.Open[c]; ok && (!found || o.At.After(item.At)) {
			best, item, found = c, o, true
		}
	}
	return best, item, found
}
