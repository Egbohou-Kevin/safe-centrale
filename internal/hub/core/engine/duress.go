package engine

import (
	"slices"
	"time"
)

// duress raises the alert of a disarm made with a duress code (23 November 2026). The
// disarm itself is the person's own (disarmBy): the same mode, reply and events, so
// that nothing on site shows the threat. The alert has an incident of its own: no
// Ended carries it, no disarm closes it at the centre, no siren rings for it. Silencing
// or ending with a duress code raises it too: both clear the alarm at the centre
// (Cleared, Ended), so without it a person forced to stop the siren would leave the
// centre with nothing open.
//
// The engine keeps it (Snapshot.Duress) until the escalation holds it on disk
// (DuressHeld), and Boot raises it again meanwhile: the effects leave before the state
// is saved, so a power cut right after the code would otherwise leave a plain disarm
// on disk and nothing at the centre (23 November 2026, docs/engine.md).
func (e *Engine) duress(origin Origin, user string) []Action {
	g := DuressGuard{Incident: e.newID(), Origin: origin, User: user}
	e.s.Duress = append(e.s.Duress, g)
	return g.raise()
}

// DuressGuard is a duress alert raised that the escalation does not hold on disk yet.
type DuressGuard struct {
	Incident string `json:"incident"`
	Origin   Origin `json:"origin,omitempty"`
	User     string `json:"user,omitempty"` // the person whose duress code was typed
}

// raise is the duress alert, the same raised live and again at Boot.
func (g DuressGuard) raise() []Action {
	return []Action{
		Emit{DuressRaised{Incident: g.Incident, Origin: g.Origin}},
		Alert{Incident: g.Incident, Kind: AlertDuress, Urgency: urgency(AlertDuress), User: g.User},
	}
}

// DuressHeld tells the engine that the escalation holds a duress alert on disk: the
// engine keeps it no more, and a restart no longer raises it. An incident the engine
// does not keep is ignored.
type DuressHeld struct {
	Now      time.Time
	Incident string
}

func (d DuressHeld) at() time.Time { return d.Now }

// Held drops the duress alert of this incident (see DuressHeld). The hub calls it
// before the loop runs too, for the alerts the escalation's saved state holds.
func (e *Engine) Held(incident string) {
	e.s.Duress = slices.DeleteFunc(e.s.Duress, func(g DuressGuard) bool { return g.Incident == incident })
	if len(e.s.Duress) == 0 {
		e.s.Duress = nil
	}
}

// raiseKept raises again the duress alerts the escalation did not hold before the
// restart, with their incident: one it holds after all groups it (escalation.raise).
func (e *Engine) raiseKept() []Action {
	var out []Action
	for _, g := range e.s.Duress {
		out = append(out, g.raise()...)
	}
	return out
}
