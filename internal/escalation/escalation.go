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
	"fmt"
	"maps"
	"slices"
	"time"
)

// Snapshot is the full state, saved by the driver after every change so that a
// restart goes on with the incidents in progress instead of forgetting them.
type Snapshot struct {
	NextKey    uint64                  `json:"next_key"`
	UplinkDown bool                    `json:"uplink_down"`
	Incidents  map[string]IncidentSnap `json:"incidents"` // keyed by incident and kind
	// Recent holds when each contact was last sent a message, for the hourly limit.
	Recent map[string][]time.Time `json:"recent"`
}

// IncidentSnap is the state of one alert being escalated.
type IncidentSnap struct {
	ID       string    `json:"id"`
	Kind     AlertKind `json:"kind"`
	Urgency  Urgency   `json:"urgency"`
	Devices  []string  `json:"devices"`
	Percent  int       `json:"percent"`
	RaisedAt time.Time `json:"raised_at"`

	Step         int                 `json:"step"`          // steps already run
	NextAt       time.Time           `json:"next_at"`       // when the next step is due, zero if none
	WaveContacts []string            `json:"wave_contacts"` // who the last step told
	Rounds       map[string]Round    `json:"rounds"`        // contact -> latest round of messages
	Deliveries   map[uint64]Delivery `json:"deliveries"`
}

// Round is the messages of one step to one contact, and the timers that go with
// them.
type Round struct {
	Step    int       `json:"step"`
	Started time.Time `json:"started"`
	Sent    []Channel `json:"sent"` // the channels used in this round
	NetOK   bool      `json:"net_ok"`
	// NetHeld: the internet message was held back because the uplink was down.
	NetHeld     bool      `json:"net_held"`
	NetDeadline time.Time `json:"net_deadline"` // the cloud must have confirmed by then
	GSMDue      time.Time `json:"gsm_due"`      // when the GSM channels take over or double
}

// Delivery is one message sent, for matching its result.
type Delivery struct {
	Contact string       `json:"contact"`
	Channel Channel      `json:"channel"`
	Step    int          `json:"step"`
	Status  ResultStatus `json:"status"`
}

func (s Snapshot) clone() Snapshot {
	out := Snapshot{NextKey: s.NextKey, UplinkDown: s.UplinkDown}
	out.Incidents = make(map[string]IncidentSnap, len(s.Incidents))
	for k, in := range s.Incidents {
		in.Devices = slices.Clone(in.Devices)
		in.WaveContacts = slices.Clone(in.WaveContacts)
		rounds := make(map[string]Round, len(in.Rounds))
		for c, r := range in.Rounds {
			r.Sent = slices.Clone(r.Sent)
			rounds[c] = r
		}
		in.Rounds = rounds
		in.Deliveries = maps.Clone(in.Deliveries)
		if in.Deliveries == nil {
			in.Deliveries = map[uint64]Delivery{}
		}
		out.Incidents[k] = in
	}
	out.Recent = make(map[string][]time.Time, len(s.Recent))
	for c, ts := range s.Recent {
		out.Recent[c] = slices.Clone(ts)
	}
	return out
}

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
	return &Escalation{cfg: cfg, s: Snapshot{
		Incidents: map[string]IncidentSnap{}, Recent: map[string][]time.Time{},
	}}, nil
}

// Snapshot returns a copy of the current state.
func (e *Escalation) Snapshot() Snapshot { return e.s.clone() }

// Restore replaces the state with a saved one.
func (e *Escalation) Restore(s Snapshot) { e.s = s.clone() }

// Active reports how many incidents are being escalated.
func (e *Escalation) Active() int { return len(e.s.Incidents) }

// UplinkDown reports whether the cloud is considered unreachable.
func (e *Escalation) UplinkDown() bool { return e.s.UplinkDown }

func incidentKey(id string, kind AlertKind) string { return id + "/" + string(kind) }

// totalSteps is the number of steps of a plan: its waves, then its reminders.
func (p Plan) totalSteps() int { return len(p.Waves) + p.Repeat.Times }

// stepAt is when step i is due, counted from the moment of the alert.
func (p Plan) stepAt(i int) time.Duration {
	if i < len(p.Waves) {
		return p.Waves[i].After
	}
	return p.Waves[len(p.Waves)-1].After + time.Duration(i-len(p.Waves)+1)*p.Repeat.Every
}

// step describes what step i does.
func (p Plan) step(i int) (to []Audience, smsNow, double bool) {
	if i < len(p.Waves) {
		w := p.Waves[i]
		return w.To, w.SMSNow, w.Double
	}
	return []Audience{AudienceEveryone}, true, false
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
		e.result(v)
	case Ack:
		out = append(out, e.ack(v)...)
	case Uplink:
		out = append(out, e.uplink(v)...)
	case Close:
		out = append(out, e.close(v)...)
	}
	return append(out, e.advance(in.at())...)
}

// --- raising -----------------------------------------------------------------------

func (e *Escalation) raise(r Raise) {
	if !slices.Contains(allKinds, r.Kind) {
		return
	}
	key := incidentKey(r.Incident, r.Kind)
	if in, ok := e.s.Incidents[key]; ok {
		// Another detector in the same incident: it is grouped, not told again.
		in.Devices = unionSorted(in.Devices, r.Devices)
		e.s.Incidents[key] = in
		return
	}
	e.s.Incidents[key] = IncidentSnap{
		ID: r.Incident, Kind: r.Kind, Urgency: r.Urgency,
		Devices: unionSorted(nil, r.Devices), Percent: r.Percent, RaisedAt: r.Now,
		NextAt: r.Now.Add(e.cfg.plan(r.Kind).stepAt(0)),
		Rounds: map[string]Round{}, Deliveries: map[uint64]Delivery{},
	}
}

func unionSorted(a, b []string) []string {
	out := slices.Clone(a)
	for _, x := range b {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	slices.Sort(out)
	return out
}

// --- time --------------------------------------------------------------------------

// event is something that falls due: a step, an internet confirmation deadline or a
// GSM takeover.
type event struct {
	at      time.Time
	key     string
	contact string
	what    int // order at the same instant: steps first
}

const (
	evStep = iota
	evNet
	evGSM
)

// advance runs everything due by now, in order, each at its own time, so a late
// tick (hub suspended, power cut) replays the schedule instead of skipping it.
func (e *Escalation) advance(now time.Time) []Action {
	var out []Action
	for {
		ev, ok := e.nextDue(now)
		if !ok {
			return out
		}
		in := e.s.Incidents[ev.key]
		switch ev.what {
		case evStep:
			out = append(out, e.runStep(&in, ev.at)...)
		case evNet:
			r := in.Rounds[ev.contact]
			r.NetDeadline = time.Time{}
			in.Rounds[ev.contact] = r
			if !r.NetOK {
				out = append(out, e.gsm(&in, ev.contact, ev.at)...)
			}
		case evGSM:
			r := in.Rounds[ev.contact]
			r.GSMDue = time.Time{}
			in.Rounds[ev.contact] = r
			if !e.plan(in).InternetOnly || !r.NetOK {
				out = append(out, e.gsm(&in, ev.contact, ev.at)...)
			}
		}
		e.s.Incidents[ev.key] = in
		out = append(out, e.finishIfDone(ev.key)...)
	}
}

func (e *Escalation) plan(in IncidentSnap) Plan { return e.cfg.plan(in.Kind) }

// nextDue returns the earliest event due at or before now.
func (e *Escalation) nextDue(now time.Time) (event, bool) {
	var best event
	found := false
	consider := func(ev event) {
		if ev.at.IsZero() || ev.at.After(now) {
			return
		}
		if !found || ev.at.Before(best.at) ||
			(ev.at.Equal(best.at) && (ev.what < best.what ||
				(ev.what == best.what && (ev.key < best.key || (ev.key == best.key && ev.contact < best.contact))))) {
			best, found = ev, true
		}
	}
	for key, in := range e.s.Incidents {
		consider(event{at: in.NextAt, key: key, what: evStep})
		for c, r := range in.Rounds {
			consider(event{at: r.NetDeadline, key: key, contact: c, what: evNet})
			consider(event{at: r.GSMDue, key: key, contact: c, what: evGSM})
		}
	}
	return best, found
}

// runStep tells the contacts of the next step. A step nobody is left for is skipped
// at once rather than waited for.
func (e *Escalation) runStep(in *IncidentSnap, at time.Time) []Action {
	plan := e.plan(*in)
	n := plan.totalSteps()
	var out []Action
	for in.Step < n {
		step := in.Step
		in.Step++
		in.NextAt = time.Time{}
		if in.Step < n {
			in.NextAt = in.RaisedAt.Add(plan.stepAt(in.Step))
		}
		to, smsNow, double := plan.step(step)
		contacts := e.contactsOf(to)
		if len(contacts) == 0 {
			continue
		}
		in.WaveContacts = in.WaveContacts[:0]
		for _, ct := range contacts {
			in.WaveContacts = append(in.WaveContacts, ct.ID)
			out = append(out, e.notify(in, ct, step, at, plan, smsNow, double)...)
		}
		break
	}
	return out
}

// contactsOf lists the contacts of several audiences once each, in configuration
// order.
func (e *Escalation) contactsOf(to []Audience) []Contact {
	picked := map[string]bool{}
	for _, a := range to {
		for _, ct := range e.cfg.audience(a) {
			picked[ct.ID] = true
		}
	}
	var out []Contact
	for _, ct := range e.cfg.Contacts {
		if picked[ct.ID] {
			out = append(out, ct)
		}
	}
	return out
}

func (e *Escalation) finishIfDone(key string) []Action {
	in, ok := e.s.Incidents[key]
	if !ok || !in.NextAt.IsZero() {
		return nil
	}
	for _, r := range in.Rounds {
		// A message held back for the uplink is still owed.
		if !r.NetDeadline.IsZero() || !r.GSMDue.IsZero() || r.NetHeld {
			return nil
		}
	}
	return e.conclude(key, OutcomeUnanswered, "")
}

func (e *Escalation) conclude(key string, outcome Outcome, by string) []Action {
	in, ok := e.s.Incidents[key]
	if !ok {
		return nil
	}
	delete(e.s.Incidents, key)
	return []Action{Concluded{Incident: in.ID, Kind: in.Kind, Outcome: outcome, By: by}}
}

// --- sending -----------------------------------------------------------------------

// notify tells one contact: internet first, the GSM channels when the internet is
// unusable or does not confirm, and again to insist.
func (e *Escalation) notify(in *IncidentSnap, ct Contact, step int, at time.Time, plan Plan, smsNow, double bool) []Action {
	round := Round{Step: step, Started: at}
	internet, gsm := split(ct.Channels)
	var out []Action
	send := func(chs ...Channel) {
		for _, ch := range chs {
			out = append(out, e.send(in, ct, ch, &round, at)...)
		}
	}

	switch {
	case plan.InternetOnly:
		if len(internet) > 0 {
			if e.s.UplinkDown {
				round.NetHeld = true
			} else {
				send(internet...)
			}
		}
		if plan.GSMAfter > 0 && len(gsm) > 0 {
			round.GSMDue = at.Add(plan.GSMAfter)
		}
	case len(internet) == 0 || e.s.UplinkDown:
		round.NetHeld = len(internet) > 0
		send(gsm...) // the internet cannot be used: the GSM channels take over at once
	default:
		send(internet...)
		round.NetDeadline = at.Add(e.cfg.InternetConfirm)
		if smsNow && slices.Contains(gsm, ChannelSMS) {
			send(ChannelSMS)
		}
		if double {
			round.GSMDue = at.Add(e.cfg.GSMDelay)
		}
	}
	in.Rounds[ct.ID] = round
	return out
}

func split(chs []Channel) (internet, gsm []Channel) {
	for _, ch := range chs {
		if ch.internet() {
			internet = append(internet, ch)
		} else {
			gsm = append(gsm, ch)
		}
	}
	return internet, gsm
}

// gsm sends, to one contact, the GSM channels not yet used in the current round.
func (e *Escalation) gsm(in *IncidentSnap, contact string, at time.Time) []Action {
	ct, ok := e.cfg.contact(contact)
	if !ok {
		return nil
	}
	round := in.Rounds[contact]
	_, gsm := split(ct.Channels)
	var out []Action
	for _, ch := range gsm {
		if !slices.Contains(round.Sent, ch) {
			out = append(out, e.send(in, ct, ch, &round, at)...)
		}
	}
	in.Rounds[contact] = round
	return out
}

// send builds one message, unless the contact already got the hourly maximum.
func (e *Escalation) send(in *IncidentSnap, ct Contact, ch Channel, round *Round, at time.Time) []Action {
	if in.Kind != KindFire && in.Kind != KindPanic {
		recent := e.s.Recent[ct.ID][:0:0]
		for _, t := range e.s.Recent[ct.ID] {
			if at.Sub(t) < time.Hour {
				recent = append(recent, t)
			}
		}
		if len(recent) >= e.cfg.MaxPerHour {
			e.s.Recent[ct.ID] = recent
			return []Action{Throttled{Incident: in.ID, Contact: ct.ID, Channel: ch}}
		}
		e.s.Recent[ct.ID] = append(recent, at)
	}
	e.s.NextKey++
	key := e.s.NextKey
	in.Deliveries[key] = Delivery{Contact: ct.ID, Channel: ch, Step: round.Step}
	if !slices.Contains(round.Sent, ch) {
		round.Sent = append(round.Sent, ch)
	}
	return []Action{Send{
		Key: key, Incident: in.ID, Kind: in.Kind, Urgency: in.Urgency, Wave: round.Step + 1,
		Contact: ct.ID, Channel: ch, Devices: slices.Clone(in.Devices), Percent: in.Percent,
		Text: e.text(*in),
	}}
}

// --- results, acknowledgements and links ---------------------------------------------

func (e *Escalation) result(r Result) {
	for key, in := range e.s.Incidents {
		d, ok := in.Deliveries[r.Key]
		if !ok {
			continue
		}
		d.Status = r.Status
		in.Deliveries[r.Key] = d
		round, current := in.Rounds[d.Contact]
		if current && round.Step == d.Step {
			e.applyResult(&in, d, &round, r)
			in.Rounds[d.Contact] = round
			e.pullForwardIfAllUnreachable(&in, r.Now)
		}
		e.s.Incidents[key] = in
		return
	}
}

func (e *Escalation) applyResult(in *IncidentSnap, d Delivery, round *Round, r Result) {
	if !d.Channel.internet() {
		return
	}
	switch r.Status {
	case ResultSent, ResultDelivered:
		round.NetOK = true
		round.NetDeadline = time.Time{}
	case ResultFailed:
		// Every internet message of the round failed: do not wait for the deadline.
		if !round.NetOK && !roundHasPending(*in, d.Contact, round.Step, true) {
			round.NetDeadline = r.Now
		}
	}
}

// roundHasPending reports whether a message of the round has no outcome yet
// (internetOnly restricts the check to the internet channels).
func roundHasPending(in IncidentSnap, contact string, step int, internetOnly bool) bool {
	for _, d := range in.Deliveries {
		if d.Contact != contact || d.Step != step || (internetOnly && !d.Channel.internet()) {
			continue
		}
		if d.Status == "" || d.Status == ResultQueued {
			return true
		}
	}
	return false
}

// pullForwardIfAllUnreachable: when no one the last step told can be reached, the
// next step does not wait for its delay.
func (e *Escalation) pullForwardIfAllUnreachable(in *IncidentSnap, now time.Time) {
	if in.NextAt.IsZero() || !in.NextAt.After(now) || len(in.WaveContacts) == 0 {
		return
	}
	for _, c := range in.WaveContacts {
		r, ok := in.Rounds[c]
		if !ok || r.NetOK || r.NetHeld || !r.NetDeadline.IsZero() || !r.GSMDue.IsZero() ||
			roundHasPending(*in, c, r.Step, false) || !roundAllFailedOn(*in, c, r.Step, false) {
			return
		}
	}
	in.NextAt = now
}

func (e *Escalation) ack(a Ack) []Action {
	if _, ok := e.cfg.contact(a.Contact); !ok {
		return nil
	}
	var out []Action
	for _, key := range e.keysOf(a.Incident) {
		out = append(out, e.conclude(key, OutcomeAcknowledged, a.Contact)...)
	}
	return out
}

func (e *Escalation) close(c Close) []Action {
	var out []Action
	for _, key := range e.keysOf(c.Incident) {
		out = append(out, e.conclude(key, OutcomeClosed, "")...)
	}
	return out
}

// keysOf returns the keys of every kind of alert under an incident, sorted.
func (e *Escalation) keysOf(incident string) []string {
	var keys []string
	for k, in := range e.s.Incidents {
		if in.ID == incident {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// uplink records the link to the cloud. When it comes back, what the internet
// channels could not carry goes out.
func (e *Escalation) uplink(u Uplink) []Action {
	e.s.UplinkDown = !u.Up
	if !u.Up {
		return nil
	}
	var out []Action
	for _, key := range slices.Sorted(maps.Keys(e.s.Incidents)) {
		in := e.s.Incidents[key]
		for _, id := range slices.Sorted(maps.Keys(in.Rounds)) {
			round := in.Rounds[id]
			ct, ok := e.cfg.contact(id)
			if !ok || round.NetOK {
				continue
			}
			internet, _ := split(ct.Channels)
			failed := !roundHasPending(in, id, round.Step, true) && roundAllFailedOn(in, id, round.Step, true)
			if len(internet) == 0 || !(round.NetHeld || failed) {
				continue
			}
			round.NetHeld = false
			for _, ch := range internet {
				out = append(out, e.resend(&in, ct, ch, &round, u.Now)...)
			}
			if !e.plan(in).InternetOnly {
				round.NetDeadline = u.Now.Add(e.cfg.InternetConfirm)
			}
			in.Rounds[id] = round
		}
		e.s.Incidents[key] = in
	}
	return out
}

// roundAllFailedOn reports whether the round sent something and every message of it
// failed (internetOnly restricts the check to the internet channels).
func roundAllFailedOn(in IncidentSnap, contact string, step int, internetOnly bool) bool {
	n := 0
	for _, d := range in.Deliveries {
		if d.Contact != contact || d.Step != step || (internetOnly && !d.Channel.internet()) {
			continue
		}
		if d.Status != ResultFailed {
			return false
		}
		n++
	}
	return n > 0
}

// resend sends a channel again in the current round (the round's channel list
// already holds it).
func (e *Escalation) resend(in *IncidentSnap, ct Contact, ch Channel, round *Round, at time.Time) []Action {
	round.Sent = slices.DeleteFunc(round.Sent, func(c Channel) bool { return c == ch })
	return e.send(in, ct, ch, round, at)
}

// Describe gives a short account of an action, for logs.
func Describe(a Action) string {
	switch v := a.(type) {
	case Send:
		return fmt.Sprintf("send %s to %s by %s (wave %d)", v.Kind, v.Contact, v.Channel, v.Wave)
	case Throttled:
		return fmt.Sprintf("throttled %s for %s by %s", v.Incident, v.Contact, v.Channel)
	case Concluded:
		return fmt.Sprintf("concluded %s: %s", v.Incident, v.Outcome)
	}
	return fmt.Sprintf("%T", a)
}
