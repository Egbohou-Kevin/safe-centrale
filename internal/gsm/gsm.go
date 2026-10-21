// Package gsm is the domain core of the GSM gateway (see docs/gsm-gateway.md): the
// queue of SMS and calls to send, with priorities and retries, the choice of the line
// (a modem and its SIM), the watch over each modem, and the contacts' "1" answers
// that acknowledge an alert.
//
// Like the other cores it is deterministic: it does not read the clock or talk to a
// modem. It is given requests, the lines' answers and ticks that carry their own
// time, and returns what the lines must do and what must be reported. It knows
// nothing of AT commands, MQTT or the event contract.
//
// A Gateway is not safe for concurrent use: the driver must feed it one input at a
// time.
package gsm

import (
	"reflect"
	"slices"
	"time"
)

// Gateway is the domain core.
type Gateway struct {
	cfg     Config
	numbers map[string]string   // contact -> international number
	byPhone map[string][]string // international number -> contacts, sorted
	test    string
	s       Snapshot
}

// New creates a gateway with an empty queue.
func New(cfg Config) (*Gateway, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	g := &Gateway{cfg: cfg, numbers: map[string]string{}, byPhone: map[string][]string{}}
	for contact, raw := range cfg.Contacts {
		n, _ := NormalizeNumber(raw, cfg.Country)
		g.numbers[contact] = n
		g.byPhone[n] = append(g.byPhone[n], contact)
	}
	for _, contacts := range g.byPhone {
		slices.Sort(contacts)
	}
	if cfg.TestNumber != "" {
		g.test, _ = NormalizeNumber(cfg.TestNumber, cfg.Country)
	}
	g.Restore(Snapshot{})
	return g, nil
}

// Snapshot returns a copy of the current state.
func (g *Gateway) Snapshot() Snapshot { return g.s.clone() }

// Restore replaces the state with a saved one. Lines added to the configuration
// since are created; lines removed are forgotten. A job that a line was sending when
// the gateway stopped goes back to the queue: whether it left is unknown, and a
// second SMS is better than none.
func (g *Gateway) Restore(s Snapshot) {
	s = s.clone()
	lines := map[string]LineState{}
	for _, l := range g.cfg.Lines {
		st, ok := s.Lines[l.ID]
		if !ok {
			st = LineState{Operator: l.Operator}
		}
		st.free()
		st.NextCheck = time.Time{} // check every line at once
		lines[l.ID] = st
	}
	s.Lines = lines
	for i := range s.Jobs {
		s.Jobs[i].Line, s.Jobs[i].Deadline = "", time.Time{}
	}
	g.s = s
}

// Lines returns the public state of every line, in configuration order.
func (g *Gateway) Lines() []LineStatus {
	out := make([]LineStatus, 0, len(g.cfg.Lines))
	for _, l := range g.cfg.Lines {
		st := g.s.Lines[l.ID]
		out = append(out, LineStatus{
			ID: l.ID, Operator: st.Operator, Responding: st.Responding, Registered: st.Registered,
			Signal: st.Signal, Credit: st.Credit, LastTest: st.LastTest,
			LastTestOK: cloneBool(st.LastTestOK), Restarts: st.Restarts,
		})
	}
	return out
}

// Queued returns the number of jobs waiting or being sent.
func (g *Gateway) Queued() int { return len(g.s.Jobs) }

// NextWake returns when the driver must send a Tick, zero if never.
func (g *Gateway) NextWake() time.Time {
	var next time.Time
	add := func(t time.Time) {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	due := time.Unix(0, 0) // something to do at once
	for _, j := range g.s.Jobs {
		if j.Line != "" {
			add(j.Deadline) // in flight: its line's deadline comes first
			continue
		}
		add(j.NotBefore)
		add(j.Received.Add(g.cfg.MaxAge[j.Priority]))
	}
	for _, l := range g.s.Lines {
		add(l.BusyUntil)
		if l.Busy == "" {
			add(ifZero(l.NextCheck, due))
		}
	}
	if g.test != "" {
		add(ifZero(g.s.NextTest, due))
	}
	return next
}

// ifZero returns def when t is zero, t otherwise.
func ifZero(t, def time.Time) time.Time {
	if t.IsZero() {
		return def
	}
	return t
}

// Handle processes an input and returns what to do.
func (g *Gateway) Handle(in Input) []Action {
	now := in.at()
	before := g.Lines()
	out := g.timers(now)
	switch v := in.(type) {
	case Request:
		out = append(out, g.request(v)...)
	case Outcome:
		out = append(out, g.outcome(v)...)
	case DeliveryReport:
		out = append(out, g.delivery(v)...)
	case Incoming:
		out = append(out, g.incoming(v)...)
	case Probe:
		out = append(out, g.probe(v)...)
	case Cancel:
		out = append(out, g.cancel(v.Incident)...)
	case Restarted:
		g.restarted(v.Line)
	}
	out = append(out, g.schedule(now)...)
	if after := g.Lines(); !reflect.DeepEqual(before, after) {
		out = append(out, LinesChanged{Lines: after})
	}
	return out
}

func (g *Gateway) lineIDs() []string {
	ids := make([]string, 0, len(g.cfg.Lines))
	for _, l := range g.cfg.Lines {
		ids = append(ids, l.ID)
	}
	return ids
}
