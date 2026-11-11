package escalation

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

// Random sequences of alerts, results, answers, uplink changes, restarts and clock
// steps, each from a fixed seed, checked after every input: an incident under way is
// always escalating (a step or a timer ahead, or a message held for the uplink), an
// answer never brings a concluded incident back, no message of a step goes twice, and
// once the uplink is back and time runs, every incident concludes, once. A failure
// prints the seed and the sequence, which replay it. The checks see their defects
// (TestTheRandomEscalationChecksSeeTheirDefects).

const (
	escSeeds = 200
	escSteps = 80
)

// escRig drives one escalation through a random sequence and keeps what it observed.
type escRig struct {
	t         *testing.T
	e         *Escalation
	now       time.Time
	raised    []string        // incidents raised, in order
	open      []string        // keys ("incident/kind") under way
	concluded map[string]bool // keys concluded
	told      map[string]bool // "incident/kind/wave/contact/channel" sent
	keys      []uint64        // messages sent, for results
}

func newEscRig(t *testing.T) *escRig {
	return &escRig{t: t, e: newEsc(t), now: t0, concluded: map[string]bool{}, told: map[string]bool{}}
}

// escOp is one input of a sequence. retry: the input may send again the internet
// messages of a round (the uplink came back, see retryInternet).
type escOp struct {
	name  string
	retry bool
	do    func(r *escRig) []Action
}

var randomKinds = []AlertKind{
	KindIntrusion, KindFire, KindPanic, KindTamper, KindAuthFailed, KindArmingBypassed,
	KindBatteryLow, KindMainsRestored, KindDeviceOffline,
}

func pickOf[T any](rnd *rand.Rand, from ...T) T { return from[rnd.IntN(len(from))] }

// nth is the n-th item of a list that grows while the sequence runs, ok false if empty.
func nth[T any](list []T, n int) (T, bool) {
	var zero T
	if len(list) == 0 {
		return zero, false
	}
	return list[n%len(list)], true
}

func randomEscOp(rnd *rand.Rand, i int) escOp {
	n := rnd.IntN(1 << 20)
	switch x := rnd.IntN(20); {
	case x < 3:
		kind, devs := pickOf(rnd, randomKinds...), pickOf(rnd, []string{"dev_back"}, []string{"dev_a", "dev_b"}, nil)
		id := fmt.Sprintf("inc%d", i)
		return escOp{name: fmt.Sprintf("raise %s %s %v", id, kind, devs), do: func(r *escRig) []Action { return r.raise(id, kind, devs) }}
	case x == 3:
		return escOp{name: fmt.Sprintf("raise again open #%d", n), do: func(r *escRig) []Action { return r.raiseAgain(n) }}
	case x < 10:
		d := pickOf(rnd, time.Second, 10*time.Second, 30*time.Second, time.Minute, 2*time.Minute, 5*time.Minute, 10*time.Minute, time.Hour)
		return escOp{name: "advance " + d.String(), do: func(r *escRig) []Action { r.now = r.now.Add(d); return r.e.Handle(Tick{Now: r.now}) }}
	case x < 13:
		st := pickOf(rnd, ResultQueued, ResultSent, ResultDelivered, ResultFailed, ResultFailed)
		return escOp{name: fmt.Sprintf("result %s for message #%d", st, n), do: func(r *escRig) []Action {
			key, _ := nth(r.keys, n)
			return r.e.Handle(Result{Now: r.now, Key: key, Status: st})
		}}
	}
	return answerOp(rnd, n)
}

// answerOp is an answer, an uplink change, a restart or a clock step.
func answerOp(rnd *rand.Rand, n int) escOp {
	contact := pickOf(rnd, "owner", "deputy", "guard", "manager", "other", "stranger")
	answer := func(name string, in func(now time.Time, incident string) Input) escOp {
		return escOp{name: fmt.Sprintf("%s by %s of incident #%d", name, contact, n), do: func(r *escRig) []Action {
			id, _ := nth(r.raised, n)
			return r.e.Handle(in(r.now, id))
		}}
	}
	switch rnd.IntN(7) {
	case 0:
		return answer("ack", func(now time.Time, id string) Input { return Ack{Now: now, Incident: id, Contact: contact} })
	case 1:
		return answer("seen", func(now time.Time, id string) Input { return Seen{Now: now, Incident: id, Contact: contact} })
	case 2:
		return answer("close", func(now time.Time, id string) Input { return Close{Now: now, Incident: id} })
	case 3:
		return escOp{name: "uplink down", do: func(r *escRig) []Action { return r.e.Handle(Uplink{Now: r.now}) }}
	case 4:
		return escOp{name: "uplink up", retry: true, do: func(r *escRig) []Action { return r.e.Handle(Uplink{Now: r.now, Up: true}) }}
	case 5:
		return escOp{name: "restart", do: func(r *escRig) []Action { r.e = restart(r.t, r.e); return nil }}
	}
	d := pickOf(rnd, time.Minute, time.Hour, 14*time.Hour)
	return escOp{name: "clock stepped " + d.String(), do: func(r *escRig) []Action {
		r.e.Stepped(d)
		r.now = r.now.Add(d)
		return r.e.Handle(Tick{Now: r.now})
	}}
}

func (r *escRig) raise(id string, kind AlertKind, devs []string) []Action {
	r.raised = append(r.raised, id)
	r.open = append(r.open, incidentKey(id, kind))
	return r.e.Handle(Raise{Now: r.now, Incident: id, Kind: kind, Urgency: UrgencyHigh, Devices: devs})
}

// raiseAgain raises again an incident under way, with another device: it is grouped.
func (r *escRig) raiseAgain(n int) []Action {
	key, ok := nth(r.open, n)
	if !ok {
		return r.e.Handle(Tick{Now: r.now})
	}
	in := r.e.Snapshot().Incidents[key]
	return r.e.Handle(Raise{Now: r.now, Incident: in.ID, Kind: in.Kind, Urgency: in.Urgency, Devices: []string{"dev_c"}})
}

func TestRandomSequencesKeepTheEscalationInvariants(t *testing.T) {
	for seed := range uint64(escSeeds) {
		rnd := rand.New(rand.NewPCG(seed, 2))
		ops := make([]escOp, escSteps)
		for i := range ops {
			ops[i] = randomEscOp(rnd, i)
		}
		fail := func(i int, err error) {
			t.Helper()
			names := make([]string, i+1)
			for j := range names {
				names[j] = ops[j].name
			}
			t.Fatalf("seed %d, step %d: %v\nsequence:\n  %s", seed, i, err, strings.Join(names, "\n  "))
		}
		r := newEscRig(t)
		for i, op := range ops {
			if err := r.step(op); err != nil {
				fail(i, err)
			}
		}
		if err := r.finish(); err != nil {
			fail(len(ops)-1, err)
		}
	}
}

func (r *escRig) step(op escOp) error {
	as := op.do(r)
	return errors.Join(r.observe(as, op.retry), escalating(r.e.Snapshot()))
}

// observe follows the messages and the conclusions of a batch of actions: nothing of
// an incident after it concluded, no conclusion twice, no message of a step twice
// but the internet ones sent again when the uplink comes back.
func (r *escRig) observe(as []Action, retry bool) error {
	for _, a := range as {
		switch v := a.(type) {
		case Send:
			key := incidentKey(v.Incident, v.Kind)
			if r.concluded[key] {
				return fmt.Errorf("message after %s concluded: %+v", key, v)
			}
			told := fmt.Sprintf("%s/%d/%s/%s", key, v.Wave, v.Contact, v.Channel)
			if r.told[told] && !(retry && v.Channel.internet()) {
				return fmt.Errorf("step told twice: %s", told)
			}
			r.told[told] = true
			r.keys = append(r.keys, v.Key)
		case Concluded:
			key := incidentKey(v.Incident, v.Kind)
			if r.concluded[key] {
				return fmt.Errorf("%s concluded twice", key)
			}
			r.concluded[key] = true
			r.open = slices.DeleteFunc(r.open, func(k string) bool { return k == key })
		}
	}
	for key := range r.e.Snapshot().Incidents {
		if r.concluded[key] {
			return fmt.Errorf("%s concluded and under way again", key)
		}
	}
	return nil
}

// escalating checks that every incident under way has something ahead: a step or a
// timer, or an internet message held while the uplink is down.
func escalating(s Snapshot) error {
	for key, in := range s.Incidents {
		ahead := !in.NextAt.IsZero()
		for _, rd := range in.Rounds {
			ahead = ahead || !rd.NetDeadline.IsZero() || !rd.GSMDue.IsZero() || rd.NetHeld && s.UplinkDown
		}
		if !ahead {
			return fmt.Errorf("%s under way with nothing ahead: %+v", key, in)
		}
	}
	return nil
}

// finish brings the uplink back and lets time run: every incident raised concludes.
func (r *escRig) finish() error {
	if err := r.observe(r.e.Handle(Uplink{Now: r.now, Up: true}), true); err != nil {
		return err
	}
	for i := 0; !r.e.NextWake().IsZero(); i++ {
		if i == 500 {
			return fmt.Errorf("still escalating after %d wakes: %+v", i, r.e.Snapshot().Incidents)
		}
		r.now = r.e.NextWake()
		if err := r.observe(r.e.Handle(Tick{Now: r.now}), false); err != nil {
			return err
		}
	}
	return allConcluded(r.open, r.e.Snapshot())
}

func allConcluded(open []string, s Snapshot) error {
	if len(open) > 0 || len(s.Incidents) > 0 {
		return fmt.Errorf("never concluded: %v, under way: %d", open, len(s.Incidents))
	}
	return nil
}

// Each check above sees the defect it is there for: fed one, it fails; fed what the
// escalation does, it passes.
func TestTheRandomEscalationChecksSeeTheirDefects(t *testing.T) {
	r := newEscRig(t)
	first := r.raise("x", KindIntrusion, []string{"dev_back"})
	if err := r.observe(first, false); err != nil {
		t.Fatalf("a real raise: %v", err)
	}
	if r.observe(first, false) == nil {
		t.Error("a step told twice passed")
	}
	if err := escalating(r.e.Snapshot()); err != nil {
		t.Fatalf("a real escalation: %v", err)
	}
	stuck := r.e.Snapshot()
	in := stuck.Incidents["x/intrusion"]
	in.NextAt, in.Rounds = time.Time{}, map[string]Round{}
	stuck.Incidents["x/intrusion"] = in
	if escalating(stuck) == nil {
		t.Error("an incident with nothing ahead passed")
	}
	closed := r.e.Handle(Close{Now: r.now, Incident: "x"})
	if err := r.observe(closed, false); err != nil {
		t.Fatalf("a real close: %v", err)
	}
	if r.observe(closed, false) == nil || r.observe(first, true) == nil {
		t.Error("a conclusion twice, or a message after it, passed")
	}
	r.e = newEsc(t)
	r.e.Handle(Raise{Now: r.now, Incident: "x", Kind: KindIntrusion, Urgency: UrgencyHigh})
	if r.observe(nil, false) == nil {
		t.Error("an incident concluded and under way again passed")
	}
	if allConcluded(nil, Snapshot{}) != nil || allConcluded([]string{"x/intrusion"}, Snapshot{}) == nil {
		t.Error("allConcluded does not tell an incident never concluded")
	}
}
