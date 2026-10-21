// Package escalation decides who is told about a problem, by which channel, in
// which order and after how long (see docs/escalation.md). It is the fourth domain
// core next to the alarm engine, the health monitor and the access codes, built on
// the same rules.
//
// It is deterministic: it does not read the clock, write anywhere or talk to any
// channel. It is given alerts, results, acknowledgements and clock ticks that carry
// their own time, and returns the messages to send. It knows nothing of MQTT, JSON
// or the event contract: the wire subpackage translates. An architecture test
// (internal/archtest) enforces both.
//
// An Escalation is not safe for concurrent use: the driver must feed it one input
// at a time.
package escalation

import (
	"maps"
	"time"
)

// Escalation is the domain core.
type Escalation struct {
	cfg Config
	s   Snapshot
}

// New creates an escalation with nothing in progress.
func New(cfg Config) (*Escalation, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// Every kind gets its plan once here: the configured one, else the default.
	plans := DefaultPlans()
	maps.Copy(plans, cfg.Plans)
	cfg.Plans = plans
	return &Escalation{cfg: cfg, s: Snapshot{
		Incidents: map[string]IncidentSnap{}, Recent: map[string][]time.Time{},
	}}, nil
}

// Snapshot returns a copy of the current state.
func (e *Escalation) Snapshot() Snapshot { return e.s.clone() }

// Restore replaces the state with a saved one.
func (e *Escalation) Restore(s Snapshot) { e.s = s.clone() }

// Bind records the id of the request that carries message key (a Send), so that
// the request's results find the message by that id (Result.Ref), after a restart
// too. It changes the state without being an input: the driver calls it while
// carrying out the Send, on the loop's goroutine, before the state is saved. An
// unknown key is ignored.
func (e *Escalation) Bind(key uint64, ref string) {
	for id, in := range e.s.Incidents {
		if d, ok := in.Deliveries[key]; ok {
			d.Ref = ref
			in.Deliveries[key] = d
			e.s.Incidents[id] = in
			return
		}
	}
}

// NextWake returns the time at which the driver must send a Tick: the earliest
// step or timer, zero if there is none.
func (e *Escalation) NextWake() time.Time {
	var next time.Time
	for _, in := range e.s.Incidents {
		next = earliest(next, in.NextAt)
		for _, r := range in.Rounds {
			next = earliest(next, r.NetDeadline)
			next = earliest(next, r.GSMDue)
		}
	}
	return next
}

// earliest returns the earlier of two times, the zero time meaning none.
func earliest(a, b time.Time) time.Time {
	switch {
	case b.IsZero():
		return a
	case a.IsZero() || b.Before(a):
		return b
	}
	return a
}

// Handle processes an input and returns what to do. What was due before the input
// happens first, at its own time, then the input, then what the input made due.
func (e *Escalation) Handle(in Input) []Action {
	out := e.advance(in.at())
	switch v := in.(type) {
	case Raise:
		e.raise(v)
	case Result:
		out = append(out, e.result(v)...)
	case Ack:
		out = append(out, e.ack(v)...)
	case Uplink:
		out = append(out, e.uplink(v)...)
	case Close:
		out = append(out, e.close(v)...)
	}
	return append(out, e.advance(in.at())...)
}

func (e *Escalation) plan(in IncidentSnap) Plan { return e.cfg.Plans[in.Kind] }

func incidentKey(id string, kind AlertKind) string { return id + "/" + string(kind) }
