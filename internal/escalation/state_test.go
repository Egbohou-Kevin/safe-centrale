package escalation

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"
)

// --- time and state ----------------------------------------------------------------------

func TestALateTickReplaysEveryStepInOrder(t *testing.T) {
	e := newEsc(t)
	raise(e, t0, KindIntrusion)
	out := tick(e, t0.Add(3*time.Hour)) // the hub was suspended
	last := 0
	for _, a := range out {
		if s, ok := a.(Send); ok {
			if s.Wave < last {
				t.Fatalf("wave %d after wave %d: the schedule must be replayed in order", s.Wave, last)
			}
			last = s.Wave
		}
	}
	if last != 6 {
		t.Fatalf("the last wave replayed is %d, want 6 (3 waves and 3 reminders)", last)
	}
	if c := concluded(out); len(c) != 1 || c[0].Outcome != OutcomeUnanswered {
		t.Fatalf("%+v", c)
	}
}

func TestTheStateSurvivesARestart(t *testing.T) {
	run := func(e *Escalation) []string {
		var all []string
		raise(e, t0, KindIntrusion, "dev_back")
		all = append(all, sends(tick(e, t0.Add(10*time.Second)))...)
		all = append(all, sends(tick(e, t0.Add(2*time.Minute)))...)
		return all
	}
	straight := newEsc(t)
	_ = run(straight)

	// The same, with a restart between the steps, through JSON like the disk.
	a := newEsc(t)
	raise(a, t0, KindIntrusion, "dev_back")
	tick(a, t0.Add(10*time.Second))
	raw, err := json.Marshal(a.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	b := newEsc(t)
	b.Restore(snap)
	got := sends(tick(b, t0.Add(2*time.Minute)))
	want(t, "after restart", got, "owner/app/2", "guard/sms/2", "guard/call/2", "deputy/app/2")
	if !reflect.DeepEqual(straight.Snapshot().Incidents["inc1/intrusion"].Step, b.Snapshot().Incidents["inc1/intrusion"].Step) {
		t.Fatal("the restarted escalation is not where the straight one is")
	}
}

func TestSnapshotsAreIsolatedCopies(t *testing.T) {
	e := newEsc(t)
	raise(e, t0, KindIntrusion, "dev_back")
	s := e.Snapshot()
	in := s.Incidents["inc1/intrusion"]
	in.Devices[0] = "tampered"
	in.WaveContacts[0] = "tampered"
	s.Recent["deputy"][0] = time.Time{}
	again := e.Snapshot().Incidents["inc1/intrusion"]
	if again.Devices[0] != "dev_back" || again.WaveContacts[0] == "tampered" {
		t.Fatal("a snapshot must not share memory with the state")
	}
	e.Restore(s)
	s.Incidents["inc1/intrusion"].Devices[0] = "again"
	if e.Snapshot().Incidents["inc1/intrusion"].Devices[0] == "again" {
		t.Fatal("Restore must copy")
	}
}

func TestResultsForUnknownMessagesAreIgnored(t *testing.T) {
	e := newEsc(t)
	raise(e, t0, KindIntrusion)
	before := e.Snapshot()
	if out := e.Handle(Result{Now: t0, Key: 9999, Status: ResultFailed}); len(out) != 0 {
		t.Fatalf("%+v", out)
	}
	if !reflect.DeepEqual(before.Incidents["inc1/intrusion"].Deliveries, e.Snapshot().Incidents["inc1/intrusion"].Deliveries) {
		t.Fatal("an unknown result must change nothing")
	}
}

func TestAnOldRoundsResultDoesNotMoveTheCurrentOne(t *testing.T) {
	e := newEsc(t)
	first := raise(e, t0, KindIntrusion)
	k := keyOf(first, "deputy", ChannelApp)
	tick(e, t0.Add(2*time.Minute)) // wave 2: a new round for the deputy
	// The cloud confirms the old message very late: it says nothing about the new round.
	e.Handle(Result{Now: t0.Add(2*time.Minute + time.Second), Key: k, Status: ResultDelivered})
	r := e.Snapshot().Incidents["inc1/intrusion"].Rounds["deputy"]
	if r.NetOK {
		t.Fatal("a result of an earlier round must not confirm the current one")
	}
}

func TestDeterminism(t *testing.T) {
	play := func() []Action {
		e := newEsc(t)
		var all []Action
		all = append(all, raise(e, t0, KindIntrusion, "dev_back")...)
		all = append(all, e.Handle(Raise{Now: t0, Incident: "f", Kind: KindFire, Urgency: UrgencyCritical})...)
		for _, siren := range []string{"dev_siren1", "dev_siren2"} {
			all = append(all, e.Handle(Raise{Now: t0.Add(5 * time.Second), Incident: siren, Kind: KindSirenFailed,
				Urgency: UrgencyHigh, Devices: []string{siren}, Alarm: "inc1"})...)
		}
		all = append(all, e.Handle(Ack{Now: t0.Add(20 * time.Second), Incident: "inc1", Contact: "owner"})...)
		all = append(all, e.Handle(Ack{Now: t0.Add(30 * time.Second), Incident: "f", Contact: "guard"})...)
		for _, d := range []time.Duration{10 * time.Second, 70 * time.Second, 2 * time.Minute, 6 * time.Minute, time.Hour} {
			all = append(all, tick(e, t0.Add(d))...)
		}
		return all
	}
	if a, b := play(), play(); !reflect.DeepEqual(a, b) {
		t.Fatalf("two identical runs differ:\n%+v\n%+v", a, b)
	}
}

func TestMessageKeysAreUniqueAndIncreasing(t *testing.T) {
	e := newEsc(t)
	var keys []uint64
	collect := func(actions []Action) {
		for _, a := range actions {
			if s, ok := a.(Send); ok {
				keys = append(keys, s.Key)
			}
		}
	}
	collect(raise(e, t0, KindIntrusion))
	collect(tick(e, t0.Add(time.Hour)))
	if !slices.IsSorted(keys) || len(slices.Compact(slices.Clone(keys))) != len(keys) || keys[0] != 1 {
		t.Fatalf("%v", keys)
	}
}

func TestAResultFindsItsMessageByRequestIDAfterARestart(t *testing.T) {
	a := newEsc(t)
	k := keyOf(raise(a, t0, KindIntrusion), "deputy", ChannelApp)
	a.Bind(k, "REQ-DEPUTY-APP")
	a.Bind(9999, "REQ-NOBODY") // no such message: nothing to remember
	raw, err := json.Marshal(a.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	b := newEsc(t)
	b.Restore(snap)
	// The cloud confirms after the restart: the request id is all the result carries.
	b.Handle(Result{Now: t0.Add(5 * time.Second), Ref: "REQ-DEPUTY-APP", Status: ResultDelivered})
	d := b.Snapshot().Incidents["inc1/intrusion"].Deliveries[k]
	if d.Status != ResultDelivered || d.Ref != "REQ-DEPUTY-APP" {
		t.Fatalf("%+v", d)
	}
	if got := sends(tick(b, t0.Add(10*time.Second))); len(got) != 0 {
		t.Fatalf("the deputy confirmed by the app needs no SMS: %v", got)
	}
	before := b.Snapshot()
	if out := b.Handle(Result{Now: t0.Add(11 * time.Second), Ref: "REQ-NOBODY", Status: ResultFailed}); len(out) != 0 ||
		!reflect.DeepEqual(before.Incidents, b.Snapshot().Incidents) {
		t.Fatalf("a result for an unknown request must change nothing: %+v", out)
	}
}
