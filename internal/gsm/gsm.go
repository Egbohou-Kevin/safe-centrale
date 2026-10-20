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
	"cmp"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Snapshot is the full state, saved by the driver after every change: a restart
// loses no request (docs/gsm-gateway.md, "File d'envoi").
type Snapshot struct {
	NextJob  uint64               `json:"next_job"`
	Jobs     []Job                `json:"jobs"`
	Seen     []string             `json:"seen"` // the latest request ids, oldest first
	Lines    map[string]LineState `json:"lines"`
	LastGood string               `json:"last_good"` // the line that sent last
	// Pending matches delivery reports (line/reference) to requests.
	Pending  map[string]string   `json:"pending"`
	Order    []string            `json:"order"` // Pending keys, oldest first
	Open     map[string]OpenItem `json:"open"`  // contact -> the incident last sent to them
	NextTest time.Time           `json:"next_test"`
}

// Job is a request in the queue, or a test SMS.
type Job struct {
	ID        uint64    `json:"id"`
	Ref       string    `json:"ref"` // the request id, empty for a test
	Incident  string    `json:"incident"`
	Contact   string    `json:"contact"`
	Number    string    `json:"number"`
	Channel   Channel   `json:"channel"`
	Priority  Priority  `json:"priority"`
	Text      string    `json:"text"`
	Received  time.Time `json:"received"`
	Attempts  int       `json:"attempts"`   // full rounds over the lines that failed
	NotBefore time.Time `json:"not_before"` // waiting before the next round
	Tried     []string  `json:"tried"`      // lines tried in the current round
	Line      string    `json:"line"`       // the line sending it now, if any
	Deadline  time.Time `json:"deadline"`   // when that line is given up
	Reported  bool      `json:"reported"`   // a failure was already reported
	Test      bool      `json:"test"`
}

// LineState is what the gateway knows of a line.
type LineState struct {
	Operator   string    `json:"operator"`
	Responding bool      `json:"responding"`
	Registered bool      `json:"registered"`
	Signal     int       `json:"signal"`
	Credit     string    `json:"credit"`
	LastTest   time.Time `json:"last_test"`
	LastTestOK *bool     `json:"last_test_ok"`
	Restarts   int       `json:"restarts"`
	Misses     int       `json:"misses"`
	Busy       string    `json:"busy"` // "", busyJob, busyCheck or busyRestart
	BusyUntil  time.Time `json:"busy_until"`
	NextCheck  time.Time `json:"next_check"`
}

const (
	busyJob     = "job"
	busyCheck   = "check"
	busyRestart = "restart"
)

// OpenItem is the incident a contact was last told about.
type OpenItem struct {
	Incident string    `json:"incident"`
	At       time.Time `json:"at"`
}

const (
	seenSize    = 512
	pendingSize = 256
)

func (s Snapshot) clone() Snapshot {
	out := s
	out.Jobs = make([]Job, len(s.Jobs))
	for i, j := range s.Jobs {
		j.Tried = slices.Clone(j.Tried)
		out.Jobs[i] = j
	}
	out.Seen = slices.Clone(s.Seen)
	out.Lines = make(map[string]LineState, len(s.Lines))
	for id, l := range s.Lines {
		if l.LastTestOK != nil {
			v := *l.LastTestOK
			l.LastTestOK = &v
		}
		out.Lines[id] = l
	}
	out.Pending = maps.Clone(s.Pending)
	if out.Pending == nil {
		out.Pending = map[string]string{}
	}
	out.Order = slices.Clone(s.Order)
	out.Open = maps.Clone(s.Open)
	if out.Open == nil {
		out.Open = map[string]OpenItem{}
	}
	return out
}

// Gateway is the domain core.
type Gateway struct {
	cfg     Config
	numbers map[string]string // contact -> international number
	byPhone map[string]string // international number -> contact
	test    string
	s       Snapshot
}

// New creates a gateway with an empty queue.
func New(cfg Config) (*Gateway, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	g := &Gateway{cfg: cfg, numbers: map[string]string{}, byPhone: map[string]string{}}
	for contact, raw := range cfg.Contacts {
		n, _ := NormalizeNumber(raw, cfg.Country)
		g.numbers[contact] = n
		g.byPhone[n] = contact
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
		st.Busy, st.BusyUntil = "", time.Time{}
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
		ls := LineStatus{
			ID: l.ID, Operator: st.Operator, Responding: st.Responding, Registered: st.Registered,
			Signal: (st.Signal + 5) / 10 * 10, Credit: st.Credit, LastTest: st.LastTest, Restarts: st.Restarts,
		}
		if st.LastTestOK != nil {
			v := *st.LastTestOK
			ls.LastTestOK = &v
		}
		out = append(out, ls)
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
			add(l.NextCheck)
			if l.NextCheck.IsZero() {
				add(time.Unix(0, 0)) // a check is due now
			}
		}
	}
	if g.test != "" {
		add(g.s.NextTest)
		if g.s.NextTest.IsZero() {
			add(time.Unix(0, 0))
		}
	}
	return next
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
	case Restarted:
		if l, ok := g.s.Lines[v.Line]; ok && l.Busy == busyRestart {
			l.Busy, l.BusyUntil, l.NextCheck = "", time.Time{}, time.Time{}
			g.s.Lines[v.Line] = l
		}
	}
	out = append(out, g.schedule(now)...)
	if after := g.Lines(); !reflect.DeepEqual(before, after) {
		out = append(out, LinesChanged{Lines: after})
	}
	return out
}

// --- requests -------------------------------------------------------------------------

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
	number, ok := g.numbers[r.Contact]
	if !ok {
		return []Action{Report{Ref: r.ID, Status: StatusFailed, Reason: ReasonUnreachable}}
	}
	g.s.NextJob++
	g.s.Jobs = append(g.s.Jobs, Job{
		ID: g.s.NextJob, Ref: r.ID, Incident: r.Incident, Contact: r.Contact, Number: number,
		Channel: r.Channel, Priority: r.Priority, Text: r.Text, Received: r.Now,
	})
	return []Action{Report{Ref: r.ID, Status: StatusQueued}}
}

func (g *Gateway) job(id uint64) (int, bool) {
	for i, j := range g.s.Jobs {
		if j.ID == id {
			return i, true
		}
	}
	return 0, false
}

func (g *Gateway) remove(i int) { g.s.Jobs = slices.Delete(g.s.Jobs, i, i+1) }

// --- time -----------------------------------------------------------------------------

func (g *Gateway) timers(now time.Time) []Action {
	var out []Action
	// A line that did not answer in time failed.
	for _, id := range g.lineIDs() {
		l := g.s.Lines[id]
		if l.Busy == "" || now.Before(l.BusyUntil) {
			continue
		}
		switch l.Busy {
		case busyJob:
			for _, j := range g.s.Jobs {
				if j.Line == id {
					out = append(out, g.failed(j.ID, id, ReasonTimeout, now)...)
					break
				}
			}
			l = g.s.Lines[id]
			l.Busy, l.BusyUntil = "", time.Time{}
			g.s.Lines[id] = l
		case busyCheck:
			out = append(out, g.probe(Probe{Now: now, Line: id})...)
		case busyRestart:
			l.Busy, l.BusyUntil, l.NextCheck = "", time.Time{}, time.Time{}
			g.s.Lines[id] = l
		}
	}
	// Requests too old are given up: the next wave of the escalation took over.
	for i := 0; i < len(g.s.Jobs); i++ {
		j := g.s.Jobs[i]
		if j.Line != "" || now.Before(j.Received.Add(g.cfg.MaxAge[j.Priority])) {
			continue
		}
		if !j.Test && !j.Reported {
			out = append(out, Report{Ref: j.Ref, Status: StatusFailed, Reason: ReasonTimeout})
		}
		g.remove(i)
		i--
	}
	// The weekly test SMS.
	if g.test != "" {
		if g.s.NextTest.IsZero() {
			g.s.NextTest = now.Add(g.cfg.TestEvery)
		} else if !now.Before(g.s.NextTest) {
			g.s.NextTest = now.Add(g.cfg.TestEvery)
			g.s.NextJob++
			g.s.Jobs = append(g.s.Jobs, Job{
				ID: g.s.NextJob, Number: g.test, Channel: ChannelSMS, Priority: PriorityOther,
				Text: "safeHome: SMS de test, ne pas repondre.", Received: now, Test: true,
			})
		}
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

// --- scheduling -----------------------------------------------------------------------

// schedule gives work to the free lines: the most urgent jobs first (an alert must
// not wait behind a routine check), then the checks that are due on the lines left
// idle.
func (g *Gateway) schedule(now time.Time) []Action {
	var out []Action
	order := make([]int, len(g.s.Jobs))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		ja, jb := g.s.Jobs[a], g.s.Jobs[b]
		return cmp.Or(cmp.Compare(ja.Priority, jb.Priority), ja.Received.Compare(jb.Received), cmp.Compare(ja.ID, jb.ID))
	})
	for _, i := range order {
		j := &g.s.Jobs[i]
		if j.Line != "" || j.NotBefore.After(now) {
			continue
		}
		j.NotBefore = time.Time{} // its wait is over, even if no line is free yet
		line, ok := g.pickLine(j.Tried)
		if !ok {
			continue
		}
		l := g.s.Lines[line]
		timeout := g.cfg.SMSTimeout
		if j.Channel == ChannelCall {
			timeout += g.cfg.RingFor
		}
		l.Busy, l.BusyUntil = busyJob, now.Add(timeout)
		g.s.Lines[line] = l
		j.Line, j.Deadline = line, l.BusyUntil
		ex := Execute{Job: j.ID, Line: line, Channel: j.Channel, Number: j.Number, Text: j.Text}
		if j.Channel == ChannelCall {
			ex.RingFor = g.cfg.RingFor
		}
		out = append(out, ex)
	}

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
		if ok && l.Busy == "" && l.Responding && l.Registered && !slices.Contains(tried, id) {
			return id, true
		}
	}
	return "", false
}

// --- outcomes -------------------------------------------------------------------------

func (g *Gateway) outcome(o Outcome) []Action {
	if l, ok := g.s.Lines[o.Line]; ok && l.Busy == busyJob {
		l.Busy, l.BusyUntil = "", time.Time{}
		l.NextCheck = o.Now.Add(g.cfg.CheckEvery) // the line just proved itself, or not
		g.s.Lines[o.Line] = l
	}
	if !o.OK {
		return g.failed(o.Job, o.Line, o.Reason, o.Now)
	}
	i, ok := g.job(o.Job)
	if !ok {
		return nil
	}
	j := g.s.Jobs[i]
	g.remove(i)
	g.s.LastGood = o.Line
	if l, ok := g.s.Lines[o.Line]; ok {
		if l.Credit == "empty" {
			l.Credit = "ok"
		}
		g.s.Lines[o.Line] = l
	}
	if j.Test {
		l := g.s.Lines[o.Line]
		ok := true
		l.LastTest, l.LastTestOK = o.Now, &ok
		g.s.Lines[o.Line] = l
		return nil
	}
	g.s.Open[j.Contact] = OpenItem{Incident: j.Incident, At: o.Now}
	if j.Channel == ChannelSMS && o.MsgRef != "" {
		g.remember(o.Line+"/"+o.MsgRef, j.Ref)
	}
	return []Action{Report{Ref: j.Ref, Status: StatusSent}}
}

// failed handles a job a line could not send: try another line, or wait and try
// again. The first full round that fails is reported, so the escalation does not
// wait for the retries.
func (g *Gateway) failed(id uint64, line string, reason Reason, now time.Time) []Action {
	if l, ok := g.s.Lines[line]; ok {
		switch reason {
		case ReasonNoCredit:
			l.Credit = "empty"
		case ReasonNoNetwork:
			l.Registered = false
		}
		g.s.Lines[line] = l
	}
	i, ok := g.job(id)
	if !ok {
		return nil
	}
	j := &g.s.Jobs[i]
	if j.Line != "" && j.Line != line {
		return nil // a late answer: another line is sending it now
	}
	j.Line, j.Deadline = "", time.Time{}
	if !slices.Contains(j.Tried, line) {
		j.Tried = append(j.Tried, line)
	}
	if g.otherLineLeft(j.Tried) {
		return nil
	}
	// Every line failed this round.
	j.Attempts++
	j.Tried = nil
	j.NotBefore = now.Add(g.backoff(j.Attempts))
	var out []Action
	if j.Test {
		l := g.s.Lines[line]
		ko := false
		l.LastTest, l.LastTestOK = now, &ko
		g.s.Lines[line] = l
		g.remove(i) // a test is not retried: its job is to tell
		return nil
	}
	if !j.Reported {
		j.Reported = true
		out = append(out, Report{Ref: j.Ref, Status: StatusFailed, Reason: reason})
	}
	return out
}

// otherLineLeft reports whether a usable line remains untried in this round.
func (g *Gateway) otherLineLeft(tried []string) bool {
	for _, id := range g.lineIDs() {
		l := g.s.Lines[id]
		if l.Responding && l.Registered && !slices.Contains(tried, id) {
			return true
		}
	}
	return false
}

func (g *Gateway) backoff(attempts int) time.Duration {
	d := g.cfg.RetryMin
	for i := 1; i < attempts && d < g.cfg.RetryMax; i++ {
		d *= 2
	}
	return min(d, g.cfg.RetryMax)
}

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

// --- incoming SMS ---------------------------------------------------------------------

func (g *Gateway) incoming(in Incoming) []Action {
	number, err := NormalizeNumber(in.From, g.cfg.Country)
	contact, known := g.byPhone[number]
	if err != nil || !known {
		return []Action{Ignored{Line: in.Line, Why: "unknown number"}}
	}
	if strings.TrimSpace(in.Text) != "1" {
		return []Action{Ignored{Line: in.Line, Why: "unknown text"}}
	}
	open, ok := g.s.Open[contact]
	if !ok || in.Now.Sub(open.At) > g.cfg.AckWindow {
		return []Action{Ignored{Line: in.Line, Why: "nothing to acknowledge"}}
	}
	return []Action{Acknowledge{Incident: open.Incident, Contact: contact}}
}

// --- line checks ----------------------------------------------------------------------

func (g *Gateway) probe(p Probe) []Action {
	l, ok := g.s.Lines[p.Line]
	if !ok {
		return nil
	}
	if l.Busy == busyCheck {
		l.Busy, l.BusyUntil = "", time.Time{}
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

// Describe gives a short account of an action, for logs. It never contains a text
// or a number.
func Describe(a Action) string {
	switch v := a.(type) {
	case Execute:
		return fmt.Sprintf("%s job %d on %s", v.Channel, v.Job, v.Line)
	case CheckLine:
		return "check " + v.Line
	case RestartModem:
		return "restart " + v.Line
	case Report:
		return fmt.Sprintf("report %s %s %s", v.Ref, v.Status, v.Reason)
	case Acknowledge:
		return fmt.Sprintf("acknowledge %s by %s", v.Incident, v.Contact)
	case LinesChanged:
		return fmt.Sprintf("%d line(s) changed", len(v.Lines))
	case Ignored:
		return "ignored SMS on " + v.Line + ": " + v.Why
	}
	return fmt.Sprintf("%T", a)
}
