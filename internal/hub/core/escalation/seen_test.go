package escalation

import (
	"encoding/json"
	"testing"
	"time"
)

func seen(e *Escalation, at time.Time, contact string) []Action {
	return e.Handle(Seen{Now: at, Incident: "inc1", Contact: contact})
}

func noted(actions []Action) []Noted {
	var out []Noted
	for _, a := range actions {
		if n, ok := a.(Noted); ok {
			out = append(out, n)
		}
	}
	return out
}

// "Vu" in the app: the contact's phone is silenced, the escalation goes on for the
// others and tells that contact nothing more, neither the double nor the next waves
// nor the reminders.
func TestASeenContactIsToldNothingMoreAndTheOthersAre(t *testing.T) {
	e := newEsc(t)
	first := raise(e, t0, KindIntrusion, "dev_back")
	e.Handle(Result{Now: t0.Add(2 * time.Second), Key: keyOf(first, "deputy", ChannelApp), Status: ResultDelivered})

	out := seen(e, t0.Add(30*time.Second), "deputy")
	if n := noted(out); len(n) != 1 || n[0] != (Noted{Incident: "inc1", Contact: "deputy"}) {
		t.Fatalf("noted %+v", n)
	}
	if c := concluded(out); len(c) != 0 {
		t.Fatalf("seeing ends nothing: %+v", c)
	}
	if got := sends(tick(e, t0.Add(time.Minute))); len(got) != 0 {
		t.Fatalf("no double for a contact who saw it: %v", got)
	}
	want(t, "wave 2 without the deputy", sends(tick(e, t0.Add(2*time.Minute))), "owner/app/2", "guard/sms/2", "guard/call/2")
	want(t, "wave 3", sends(tick(e, t0.Add(5*time.Minute))), "owner/sms/2", "owner/call/2", "manager/whatsapp/3", "other/sms/3")
	want(t, "reminder 1 without the deputy", sends(tick(e, t0.Add(15*time.Minute))),
		"manager/sms/3", "owner/app/4", "owner/sms/4", "guard/sms/4", "guard/call/4", "manager/whatsapp/4", "manager/sms/4", "other/sms/4")

	// Seeing it again says nothing new; "Je m'en occupe" afterwards is the acknowledgement.
	if n := noted(seen(e, t0.Add(16*time.Minute), "deputy")); len(n) != 0 {
		t.Fatalf("seen twice: %+v", n)
	}
	c := concluded(e.Handle(Ack{Now: t0.Add(17 * time.Minute), Incident: "inc1", Contact: "deputy"}))
	if len(c) != 1 || c[0].Outcome != OutcomeAcknowledged {
		t.Fatalf("%+v", c)
	}
}

// The GSM takeover the internet confirmation would bring is dropped too: the app
// reached the contact.
func TestSeenDropsTheGSMTakeoverOfTheRound(t *testing.T) {
	e := newEsc(t)
	raise(e, t0, KindIntrusion)
	seen(e, t0.Add(5*time.Second), "deputy")
	if got := sends(tick(e, t0.Add(time.Minute))); len(got) != 0 {
		t.Fatalf("no SMS after the cloud's silence for a contact who saw it: %v", got)
	}
}

// A step left with only contacts who saw the incident is skipped at once, as an
// empty one is: the next people are told sooner. Once everyone saw it and nobody
// took it in hand, the escalation ends unanswered.
func TestAStepOfContactsWhoSawItIsSkipped(t *testing.T) {
	e := newEsc(t)
	raise(e, t0, KindIntrusion)
	seen(e, t0.Add(20*time.Second), "deputy")
	seen(e, t0.Add(25*time.Second), "guard")
	seen(e, t0.Add(30*time.Second), "owner")
	want(t, "wave 2 skipped, wave 3 at once", sends(tick(e, t0.Add(2*time.Minute))), "manager/whatsapp/3", "other/sms/3")
	seen(e, t0.Add(3*time.Minute), "manager")
	seen(e, t0.Add(3*time.Minute), "other")
	out := tick(e, t0.Add(15*time.Minute))
	if got := sends(out); len(got) != 0 {
		t.Fatalf("no reminder: %v", got)
	}
	if c := concluded(out); len(c) != 1 || c[0].Outcome != OutcomeUnanswered {
		t.Fatalf("every contact saw it, nobody took it in hand: %+v", c)
	}
}

func TestSeenByAStrangerOrForAnUnknownIncidentChangesNothing(t *testing.T) {
	e := newEsc(t)
	raise(e, t0, KindIntrusion)
	if out := seen(e, t0.Add(time.Second), "stranger"); len(noted(out)) != 0 {
		t.Fatalf("%+v", out)
	}
	if out := e.Handle(Seen{Now: t0.Add(time.Second), Incident: "nope", Contact: "deputy"}); len(noted(out)) != 0 {
		t.Fatalf("%+v", out)
	}
	want(t, "wave 2 untouched", sends(tick(e, t0.Add(2*time.Minute))), "deputy/sms/1", "owner/app/2", "guard/sms/2", "guard/call/2", "deputy/app/2")
}

// Who saw it is saved with the state: after a restart, they are still left out.
func TestWhoSawItSurvivesARestart(t *testing.T) {
	e := newEsc(t)
	raise(e, t0, KindIntrusion)
	seen(e, t0.Add(30*time.Second), "deputy")
	raw, err := json.Marshal(e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	again := newEsc(t)
	again.Restore(s)
	want(t, "wave 2 after a restart", sends(tick(again, t0.Add(2*time.Minute))), "owner/app/2", "guard/sms/2", "guard/call/2")
}

// A site without a GSM line never asks for an SMS or a call: a contact who only
// accepts those is never told, and the step left with nobody is skipped.
func TestASiteWithoutGSMAsksForNoSMSAndNoCall(t *testing.T) {
	e := newEsc(t, func(c *Config) { c.WithoutGSM = true })
	want(t, "wave 1", sends(raise(e, t0, KindIntrusion)), "deputy/app/1")
	if got := sends(tick(e, t0.Add(time.Minute))); len(got) != 0 {
		t.Fatalf("no GSM takeover nor double: %v", got)
	}
	want(t, "wave 2", sends(tick(e, t0.Add(2*time.Minute))), "owner/app/2", "deputy/app/2")
	want(t, "wave 3", sends(tick(e, t0.Add(5*time.Minute))), "manager/whatsapp/3")
	want(t, "reminder", sends(tick(e, t0.Add(15*time.Minute))), "owner/app/4", "deputy/app/4", "manager/whatsapp/4")

	// The uplink down: the internet message waits for it, nothing goes by GSM.
	e = newEsc(t, func(c *Config) { c.WithoutGSM = true })
	e.Handle(Uplink{Now: t0, Up: false})
	if got := sends(raise(e, t0, KindFire)); len(got) != 0 {
		t.Fatalf("nothing can go: %v", got)
	}
	want(t, "back", sends(e.Handle(Uplink{Now: t0.Add(time.Minute), Up: true})), "owner/app/1", "deputy/app/1", "manager/whatsapp/1")
}

func TestWithoutGSMTheContactsListShowsOnlyTheChannelsThatWork(t *testing.T) {
	e := newEsc(t, func(c *Config) { c.WithoutGSM = true })
	got := map[string]Told{}
	for _, told := range e.Told(KindIntrusion) {
		got[told.Contact.ID] = told
	}
	if chs := got["owner"].Contact.Channels; len(chs) != 1 || chs[0] != ChannelApp {
		t.Fatalf("owner %v", chs)
	}
	if g := got["guard"]; !g.Never || len(g.Contact.Channels) != 0 {
		t.Fatalf("a guard with SMS and calls only is never told: %+v", g)
	}
	if o := got["other"]; !o.Never {
		t.Fatalf("%+v", o)
	}
}
