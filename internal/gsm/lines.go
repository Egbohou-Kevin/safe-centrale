package gsm

import (
	"strings"
	"time"
)

// probe takes the answer to a line check. A modem silent for MissesToRestart checks
// in a row is restarted.
func (g *Gateway) probe(p Probe) []Action {
	l, ok := g.s.Lines[p.Line]
	if !ok {
		return nil
	}
	if l.Busy == busyCheck {
		l.free()
	}
	l.NextCheck = p.Now.Add(g.cfg.CheckEvery)
	var out []Action
	if p.Responding {
		l.Responding, l.Registered, l.Signal, l.Misses = true, p.Registered, min(max(p.Signal, 0), 100), 0
		if p.Operator != "" {
			l.Operator = p.Operator
		}
	} else {
		l.Responding, l.Registered = false, false
		l.Misses++
		if l.Misses >= g.cfg.MissesToRestart && l.Busy == "" {
			l.Misses = 0
			l.Restarts++
			l.Busy, l.BusyUntil = busyRestart, p.Now.Add(g.cfg.RestartTimeout)
			out = append(out, RestartModem{Line: p.Line})
		}
	}
	g.s.Lines[p.Line] = l
	return out
}

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

// incoming turns a contact's "1" into the acknowledgement of the incident they
// were last told about, within AckWindow.
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
	return []Action{Acknowledge{Incident: open.Incident, Contact: contact}}
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
