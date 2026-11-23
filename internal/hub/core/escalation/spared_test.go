package escalation

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// spareRun raises a duress sparing the given contact, then ticks every minute for
// three hours: every wave, reminder and GSM takeover falls in. It returns every
// action, so that a message on any channel, a withheld call or a throttled one shows.
func spareRun(e *Escalation, spared ...string) []Action {
	out := e.Handle(Raise{Now: t0, Incident: "d", Kind: KindDuress, Urgency: UrgencyCritical, Spared: spared})
	for m := 1; m <= 180; m++ {
		out = append(out, tick(e, t0.Add(time.Duration(m)*time.Minute))...)
	}
	return out
}

// toldTo lists everything an incident did to one contact.
func toldTo(actions []Action, incident, contact string) []string {
	var out []string
	for _, a := range actions {
		switch x := a.(type) {
		case Send:
			if x.Incident == incident && x.Contact == contact {
				out = append(out, fmt.Sprintf("send %s/%d", x.Channel, x.Wave))
			}
		case Withheld:
			if x.Incident == incident && x.Contact == contact {
				out = append(out, "withheld "+string(x.Channel))
			}
		case Throttled:
			if x.Incident == incident && x.Contact == contact {
				out = append(out, "throttled "+string(x.Channel))
			}
		}
	}
	return out
}

// The person who typed the duress code is told nothing of the alert they raised: not
// in the first wave, not by a reminder, on no channel. Their phone may be in the
// hands of whoever threatens them. The others are told as before.
func TestTheSparedContactIsNeverToldOfTheDuress(t *testing.T) {
	out := spareRun(newEsc(t), "owner")
	if got := toldTo(out, "d", "owner"); len(got) != 0 {
		t.Fatalf("the person under threat was told: %v", got)
	}
	plain := spareRun(newEsc(t), "")
	for _, id := range []string{"deputy", "guard", "manager", "other"} {
		want(t, id, toldTo(out, "d", id), toldTo(plain, "d", id)...)
	}
	if len(toldTo(plain, "d", "owner")) < 4 {
		t.Fatalf("without Spared the owner must be told at once and reminded: %v", toldTo(plain, "d", "owner"))
	}
}

// When the spared person is the site's only contact, the hub tells nobody: the
// monitoring centre is told by the cloud (alarm.triggered, lot D2c), not here.
func TestASparedOnlyContactLeavesTheHubTellingNobody(t *testing.T) {
	e := newEsc(t, func(c *Config) { c.Contacts = c.Contacts[:1] })
	out := spareRun(e, "owner")
	if got := sends(out); len(got) != 0 {
		t.Fatalf("the only contact is the person under threat, yet: %v", got)
	}
	if got := toldTo(out, "d", "owner"); len(got) != 0 {
		t.Fatalf("the person under threat was told: %v", got)
	}
	if c := concluded(out); len(c) != 1 || c[0].Outcome != OutcomeUnanswered {
		t.Fatalf("the duress must conclude, nobody told: %+v", c)
	}
}

// Spared is saved with the incident: after a restart, the reminders still spare the
// person.
func TestSparedSurvivesARestart(t *testing.T) {
	a := newEsc(t)
	first := a.Handle(Raise{Now: t0, Incident: "d", Kind: KindDuress, Urgency: UrgencyCritical, Spared: []string{"owner"}})
	raw, err := json.Marshal(a.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"spared":["owner"]`) {
		t.Fatalf("the snapshot does not keep Spared: %s", raw)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if got := snap.Incidents[incidentKey("d", KindDuress)].Spared; len(got) != 1 || got[0] != "owner" {
		t.Fatalf("Spared after the round trip: %q", got)
	}
	b := newEsc(t)
	b.Restore(snap)
	out := first
	for m := 1; m <= 180; m++ {
		out = append(out, tick(b, t0.Add(time.Duration(m)*time.Minute))...)
	}
	if got := toldTo(out, "d", "owner"); len(got) != 0 {
		t.Fatalf("after a restart the person under threat was told: %v", got)
	}
	if got := toldTo(out, "d", "deputy"); len(got) < 4 {
		t.Fatalf("after a restart the others must still be reminded: %v", got)
	}
}

// A Spared that is empty or names no contact changes nothing, and is no error.
func TestAnEmptyOrUnknownSparedChangesNothing(t *testing.T) {
	plain := sends(spareRun(newEsc(t), ""))
	for _, spared := range []string{"", "nobody"} {
		got := sends(spareRun(newEsc(t), spared))
		want(t, fmt.Sprintf("spared %q", spared), got, plain...)
	}
}

// Spared holds for the alert that carries it only: another alert of the site, raised
// meanwhile, tells that contact as before.
func TestAnotherAlertStillTellsTheSparedContact(t *testing.T) {
	e := newEsc(t)
	out := e.Handle(Raise{Now: t0, Incident: "d", Kind: KindDuress, Urgency: UrgencyCritical, Spared: []string{"owner"}})
	out = append(out, e.Handle(Raise{Now: t0.Add(time.Second), Incident: "p", Kind: KindPanic, Urgency: UrgencyCritical})...)
	out = append(out, e.Handle(Raise{Now: t0.Add(2 * time.Second), Incident: "d2", Kind: KindDuress, Urgency: UrgencyCritical})...)
	for m := 1; m <= 30; m++ {
		out = append(out, tick(e, t0.Add(time.Duration(m)*time.Minute))...)
	}
	if got := toldTo(out, "d", "owner"); len(got) != 0 {
		t.Fatalf("the spared duress told its person: %v", got)
	}
	for _, inc := range []string{"p", "d2"} {
		got := toldTo(out, inc, "owner")
		for _, w := range []string{"send app/1", "send sms/1", "send call/1"} {
			if !strings.Contains(strings.Join(got, ","), w) {
				t.Fatalf("%s must tell the owner as before (%s): %v", inc, w, got)
			}
		}
	}
}

// tickFor ticks every minute from t0 for three hours, after the raises.
func tickFor(e *Escalation) []Action {
	var out []Action
	for m := 1; m <= 180; m++ {
		out = append(out, tick(e, t0.Add(time.Duration(m)*time.Minute))...)
	}
	return out
}

// A second person types their duress code in the same incident: the raise is merged
// into the open alert, and spares them too. Already told by the first wave, they get
// nothing more: no reminder, no GSM takeover. The first person stays spared.
func TestAMergedDuressSparesItsPersonToo(t *testing.T) {
	e := newEsc(t)
	e.Handle(Raise{Now: t0, Incident: "d", Kind: KindDuress, Urgency: UrgencyCritical, Spared: []string{"owner"}})
	after := e.Handle(Raise{Now: t0.Add(time.Second), Incident: "d", Kind: KindDuress, Urgency: UrgencyCritical, Spared: []string{"deputy"}})
	want(t, "spared", e.Snapshot().Incidents[incidentKey("d", KindDuress)].Spared, "deputy", "owner")
	after = append(after, tickFor(e)...)
	for _, id := range []string{"owner", "deputy"} {
		if got := toldTo(after, "d", id); len(got) != 0 {
			t.Fatalf("%s, under threat, was told after the merge: %v", id, got)
		}
	}
	if got := toldTo(after, "d", "guard"); len(got) < 3 {
		t.Fatalf("the others must still be reminded: %v", got)
	}
}

// A first raise without Spared (the code not yet known), then a retry that names the
// person: from the merge on, that person is told nothing more, and the alert still
// concludes.
func TestALateSparedStopsTellingThePerson(t *testing.T) {
	e := newEsc(t)
	e.Handle(Raise{Now: t0, Incident: "d", Kind: KindDuress, Urgency: UrgencyCritical})
	after := e.Handle(Raise{Now: t0.Add(time.Second), Incident: "d", Kind: KindDuress, Urgency: UrgencyCritical, Spared: []string{"owner"}})
	after = append(after, tickFor(e)...)
	if got := toldTo(after, "d", "owner"); len(got) != 0 {
		t.Fatalf("the person under threat was told after the late Spared: %v", got)
	}
	if got := toldTo(after, "d", "deputy"); len(got) < 3 {
		t.Fatalf("the others must still be reminded: %v", got)
	}
	if c := concluded(after); len(c) != 1 {
		t.Fatalf("the duress must still conclude: %+v", c)
	}
}

// Every contact spared after the first wave: nothing is owed any more, and the alert
// concludes at once rather than waiting on rounds nobody may be told by.
func TestSparingEveryoneLateConcludesTheAlert(t *testing.T) {
	e := newEsc(t, func(c *Config) { c.Contacts = c.Contacts[:1] })
	e.Handle(Raise{Now: t0, Incident: "d", Kind: KindDuress, Urgency: UrgencyCritical})
	after := e.Handle(Raise{Now: t0.Add(time.Second), Incident: "d", Kind: KindDuress, Urgency: UrgencyCritical, Spared: []string{"owner"}})
	after = append(after, tickFor(e)...)
	if got := toldTo(after, "d", "owner"); len(got) != 0 {
		t.Fatalf("the only contact, under threat, was told after the merge: %v", got)
	}
	if c := concluded(after); len(c) != 1 || c[0].Outcome != OutcomeUnanswered {
		t.Fatalf("the duress must conclude: %+v", c)
	}
}
