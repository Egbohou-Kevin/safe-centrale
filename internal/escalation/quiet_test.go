package escalation

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// --- a silent siren during an alarm ------------------------------------------------------

// sirenFails raises a silent siren under an incident of its own, part of an alarm.
func sirenFails(e *Escalation, at time.Time, id, alarm string, siren ...string) []Action {
	if len(siren) == 0 {
		siren = []string{"dev_siren1"}
	}
	return e.Handle(Raise{Now: at, Incident: id, Kind: KindSirenFailed, Urgency: UrgencyHigh, Devices: siren, Alarm: alarm})
}

// intrusion raises an intrusion, the alarm a siren sounds for.
func intrusion(e *Escalation, at time.Time, id string) []Action {
	return e.Handle(Raise{Now: at, Incident: id, Kind: KindIntrusion, Urgency: UrgencyCritical, Devices: []string{"dev_back"}})
}

func ackBy(e *Escalation, at time.Time, incident, contact string) []Action {
	return e.Handle(Ack{Now: at, Incident: incident, Contact: contact})
}

// sendsOf returns the messages of one incident, without their keys: twin
// escalations number their messages apart.
func sendsOf(actions []Action, incident string) []Send {
	var out []Send
	for _, a := range actions {
		if s, ok := a.(Send); ok && s.Incident == incident {
			s.Key = 0
			out = append(out, s)
		}
	}
	return out
}

func withheld(actions []Action) []string {
	var out []string
	for _, a := range actions {
		if w, ok := a.(Withheld); ok {
			out = append(out, w.Contact+"/"+string(w.Channel))
		}
	}
	return out
}

// mustBeQuiet checks that every message is a quiet one of its alarm: no call, urgency
// low, the alarm named and said handled, nothing to answer.
func mustBeQuiet(t *testing.T, msgs []Send, alarm string) {
	t.Helper()
	if len(msgs) == 0 {
		t.Fatal("nothing was told")
	}
	for _, s := range msgs {
		if s.Channel == ChannelCall || s.Urgency != UrgencyLow || s.Alarm != alarm || !s.AlarmHandled ||
			strings.Contains(s.Text, "Repondre") {
			t.Fatalf("not a quiet message: %+v", s)
		}
	}
}

func outcomeOf(t *testing.T, actions []Action, incident string) Outcome {
	t.Helper()
	for _, c := range concluded(actions) {
		if c.Incident == incident {
			return c.Outcome
		}
	}
	t.Fatalf("%s is not concluded: %+v", incident, actions)
	return ""
}

// Before anyone handles the alarm, the siren's alert names it and is otherwise the
// one sent today; the alarm's own escalation is not touched.
func TestASilentSirenBeforeAnyAnswerGoesOnAsToday(t *testing.T) {
	play := func(siren bool, alarm string) []Action {
		e := newEsc(t)
		out := intrusion(e, t0, "x")
		if siren {
			out = append(out, sirenFails(e, t0.Add(30*time.Second), "y", alarm)...)
		}
		return append(out, tick(e, t0.Add(time.Hour))...) // replayed in order, each at its time
	}
	alone, named := play(true, ""), play(true, "x")
	for i, a := range named {
		if s, ok := a.(Send); ok && s.Incident == "y" {
			if s.Alarm != "x" || s.AlarmHandled {
				t.Fatalf("%+v", s)
			}
			s.Alarm = ""
			named[i] = s
		}
	}
	if !reflect.DeepEqual(alone, named) {
		t.Fatalf("naming the alarm changed the escalation:\n%+v\n%+v", alone, named)
	}
	if want := sendsOf(play(false, ""), "x"); !reflect.DeepEqual(sendsOf(named, "x"), want) {
		t.Fatalf("the alarm's waves and reminders changed:\n%+v\n%+v", sendsOf(named, "x"), want)
	}
	if !slices.Contains(sends(named), "guard/call/1") {
		t.Fatalf("the guard is called for the siren: %v", sends(named))
	}
}

// A tamper's escalation usually ends unanswered: nobody handled it, the siren's alert
// stays loud whenever the siren fails.
func TestAnAlarmThatEndedUnansweredLeavesTheSirenAlertLoud(t *testing.T) {
	e := newEsc(t)
	e.Handle(Raise{Now: t0, Incident: "t", Kind: KindTamper, Urgency: UrgencyHigh})
	if outcomeOf(t, tick(e, t0.Add(2*time.Minute)), "t") != OutcomeUnanswered {
		t.Fatal("the tamper must end unanswered")
	}
	out := sirenFails(e, t0.Add(3*time.Minute), "y", "t")
	want(t, "loud", sends(out), "deputy/app/1", "guard/sms/1", "guard/call/1")
	for _, s := range sendsOf(out, "y") {
		if s.Urgency != UrgencyHigh || s.AlarmHandled || s.Alarm != "t" {
			t.Fatalf("%+v", s)
		}
	}
}

// A fire raised during the intrusion has no siren of its own: while nobody answered
// it, its people may count on the silent siren, which stays loud.
func TestALiveFireKeepsTheSirenAlertLoud(t *testing.T) {
	e := newEsc(t)
	intrusion(e, t0, "x")
	e.Handle(Raise{Now: t0, Incident: "f", Kind: KindFire, Urgency: UrgencyCritical, Devices: []string{"dev_smoke"}})
	ackBy(e, t0.Add(10*time.Second), "x", "owner")
	loud := sirenFails(e, t0.Add(20*time.Second), "y", "x")
	if !slices.Contains(sends(loud), "guard/call/1") {
		t.Fatalf("the fire is still open, the guard is called: %v", sends(loud))
	}

	// The fire is acknowledged: the siren's alert becomes quiet, the rest goes as is.
	if out := ackBy(e, t0.Add(25*time.Second), "f", "manager"); len(sends(out)) != 0 || outcomeOf(t, out, "f") != OutcomeAcknowledged {
		t.Fatalf("%+v", out)
	}
	rest := tick(e, t0.Add(time.Hour))
	mustBeQuiet(t, sendsOf(rest, "y"), "x")
	want(t, "the fact by SMS", sends(rest), "deputy/sms/1")
	if outcomeOf(t, rest, "y") != OutcomeInformed {
		t.Fatalf("%+v", rest)
	}
}

// The trial of 24 October 2026, in the core: the alarm was acknowledged, then the
// siren failed. Nobody is rung again: not by call, link down or not, nor a contact
// without internet. A contact who only takes calls gets nothing, which is reported.
func TestASilentSirenInAHandledAlarmNeverCalls(t *testing.T) {
	for _, linkDown := range []bool{false, true} {
		e := newEsc(t, func(c *Config) {
			c.Contacts = []Contact{
				{ID: "owner", Role: RoleOwner, Channels: []Channel{ChannelApp, ChannelSMS, ChannelCall}},
				{ID: "guard", Role: RoleGuard, Channels: []Channel{ChannelCall}},
			}
		})
		intrusion(e, t0, "x")
		ackBy(e, t0.Add(5*time.Second), "x", "owner")
		e.Handle(Uplink{Now: t0.Add(6 * time.Second), Up: !linkDown})
		out := sirenFails(e, t0.Add(10*time.Second), "y", "x")
		out = append(out, tick(e, t0.Add(time.Hour))...)
		if linkDown { // the SMS went at once; the app's message waits for the link
			want(t, "told", sends(out), "owner/sms/1")
			out = append(out, e.Handle(Uplink{Now: t0.Add(time.Hour), Up: true})...)
			out = append(out, tick(e, t0.Add(2*time.Hour))...)
		}
		want(t, "told", sends(out), "owner/app/1", "owner/sms/1")
		mustBeQuiet(t, sendsOf(out, "y"), "x")
		want(t, "withheld once each", withheld(out), "owner/call", "guard/call")
		if outcomeOf(t, out, "y") != OutcomeInformed {
			t.Fatalf("%+v", out)
		}
	}
}

// A disarm, or the siren stopped with a code, handles the alarm as an
// acknowledgement does; the text says it was stopped.
func TestADisarmCountsAsHandled(t *testing.T) {
	e := newEsc(t)
	intrusion(e, t0, "x")
	e.Handle(Close{Now: t0.Add(time.Minute), Incident: "x"})
	ackBy(e, t0.Add(70*time.Second), "x", "owner") // a late "1" does not overwrite it
	out := sirenFails(e, t0.Add(2*time.Minute), "y", "x")
	mustBeQuiet(t, sendsOf(out, "y"), "x")
	if text := sendsOf(out, "y")[0].Text; !strings.HasSuffix(text, "L'alarme a ete arretee.") {
		t.Fatalf("%q", text)
	}
}

// After the alarm is handled, the fact still reaches each phone the internet missed,
// by SMS: a message that failed, one the cloud did not take in time, one it took but
// the phone never said it got. Never a call.
func TestTheQuietFactStillReachesWhoTheInternetMissed(t *testing.T) {
	for name, c := range map[string]struct {
		status ResultStatus // the cloud's answer at +1 s, none if empty
		sms    time.Duration
	}{
		"failed": {ResultFailed, time.Second}, "unconfirmed": {"", 10 * time.Second},
		"sent": {ResultSent, time.Minute}, "delivered": {ResultDelivered, 0},
	} {
		e := newEsc(t, func(c *Config) {
			c.Contacts = []Contact{{ID: "owner", Role: RoleOwner, Channels: []Channel{ChannelApp, ChannelSMS, ChannelCall}}}
		})
		intrusion(e, t0, "x")
		ackBy(e, t0, "x", "owner")
		out := sirenFails(e, t0, "y", "x")
		app := keyOf(out, "owner", ChannelApp)
		if c.status != "" {
			out = append(out, e.Handle(Result{Now: t0.Add(time.Second), Key: app, Status: c.status})...)
		}
		for at := time.Second; at <= time.Hour; at += time.Second {
			got := tick(e, t0.Add(at))
			if s := sends(got); len(s) > 0 && (at != c.sms || !slices.Equal(s, []string{"owner/sms/1"})) {
				t.Fatalf("%s: %v at +%v, want one SMS at +%v", name, s, at, c.sms)
			}
			out = append(out, got...)
		}
		if wantSMS := c.sms != 0; slices.Contains(sends(out), "owner/sms/1") != wantSMS {
			t.Fatalf("%s: %v", name, sends(out))
		}
		mustBeQuiet(t, sendsOf(out, "y"), "x")
	}
}

// The alarm is acknowledged after the siren's alert left: from then on it is quiet.
// What left is not taken back, nothing else is concluded for it.
func TestAnAcknowledgementAfterTheSirenFailedQuietsIt(t *testing.T) {
	e := newEsc(t)
	intrusion(e, t0, "x")
	loud := sirenFails(e, t0.Add(5*time.Second), "y", "x")
	want(t, "loud", sends(loud), "deputy/app/1", "guard/sms/1", "guard/call/1")
	e.Handle(Result{Now: t0.Add(6 * time.Second), Key: keyOf(loud, "deputy", ChannelApp), Status: ResultSent})

	acked := ackBy(e, t0.Add(20*time.Second), "x", "owner")
	if c := concluded(acked); len(sendsOf(acked, "y")) != 0 || len(c) != 1 || c[0].Incident != "x" || c[0].Outcome != OutcomeAcknowledged {
		t.Fatalf("only the alarm is concluded: %+v", acked)
	}
	// At the doubling, the deputy's phone never said it got the message: an SMS, no call.
	later := tick(e, t0.Add(5*time.Second+time.Minute))
	want(t, "the doubling", sends(later), "deputy/sms/1")
	mustBeQuiet(t, sendsOf(later, "y"), "x")
	if outcomeOf(t, later, "y") != OutcomeInformed {
		t.Fatalf("%+v", later)
	}
}

func TestTheSirenAlertIsNeverThrottled(t *testing.T) {
	e := newEsc(t, func(c *Config) { c.MaxPerHour = 2 })
	intrusion(e, t0, "x")
	tick(e, t0.Add(10*time.Second)) // the deputy's SMS: two messages this hour
	ackBy(e, t0.Add(20*time.Second), "x", "deputy")
	out := sirenFails(e, t0.Add(30*time.Second), "y", "x")
	out = append(out, tick(e, t0.Add(time.Hour))...)
	want(t, "told", sends(out), "deputy/app/1", "deputy/sms/1", "guard/sms/1")
	for _, a := range out {
		if _, ok := a.(Throttled); ok {
			t.Fatalf("%+v", a)
		}
	}
}

// Another alarm and a fire keep every message they would have had: quieting the
// siren's alert touches nothing else.
func TestQuietingTouchesNoOtherIncident(t *testing.T) {
	for _, fire := range []bool{false, true} {
		play := func(siren bool) []Action {
			e := newEsc(t)
			out := intrusion(e, t0, "x")
			out = append(out, intrusion(e, t0.Add(time.Second), "x2")...)
			if fire {
				out = append(out, e.Handle(Raise{Now: t0.Add(2 * time.Second), Incident: "f", Kind: KindFire, Urgency: UrgencyCritical})...)
			}
			if siren {
				out = append(out, sirenFails(e, t0.Add(3*time.Second), "y", "x")...)
			}
			out = append(out, ackBy(e, t0.Add(4*time.Second), "x", "owner")...)
			return append(out, tick(e, t0.Add(time.Hour))...)
		}
		with, without := play(true), play(false)
		for _, id := range []string{"x", "x2", "f"} {
			if !reflect.DeepEqual(sendsOf(with, id), sendsOf(without, id)) {
				t.Fatalf("fire %v: %s changed:\n%+v\n%+v", fire, id, sendsOf(with, id), sendsOf(without, id))
			}
		}
		quiet := slices.ContainsFunc(sendsOf(with, "y"), func(s Send) bool { return s.AlarmHandled })
		if quiet == fire {
			t.Fatalf("fire %v: the siren's alert became quiet: %v", fire, quiet)
		}
	}
}

// A site's plan may give the siren more than one wave: a quiet alert is told once,
// and one that turns quiet stops after the step it already ran.
func TestAQuietAlertIsToldOnce(t *testing.T) {
	plans := DefaultPlans()
	plans[KindSirenFailed] = Plan{
		Waves:  []Wave{{To: []Audience{AudienceFirst}}, {After: 2 * time.Minute, To: []Audience{AudienceEveryone}}},
		Repeat: Repeat{Every: 10 * time.Minute, Times: 3},
	}
	e := newEsc(t, func(c *Config) { c.Plans = plans })
	intrusion(e, t0, "x")
	sirenFails(e, t0, "y1", "x")
	rest := ackBy(e, t0.Add(time.Minute), "x", "owner")
	rest = append(rest, sirenFails(e, t0.Add(time.Minute), "y2", "x", "dev_siren2")...)
	rest = append(rest, tick(e, t0.Add(2*time.Hour))...)
	for _, id := range []string{"y1", "y2"} {
		for _, s := range sendsOf(rest, id) {
			if s.Wave != 1 {
				t.Fatalf("%s: %+v", id, s)
			}
		}
	}
	if outcomeOf(t, rest, "y1") != OutcomeInformed || outcomeOf(t, rest, "y2") != OutcomeInformed {
		t.Fatalf("%+v", concluded(rest))
	}
}

// --- what is remembered ------------------------------------------------------------------

// restart saves the state as the driver does, through JSON, into a new escalation.
func restart(t *testing.T, e *Escalation) *Escalation {
	t.Helper()
	raw, err := json.Marshal(e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	b := newEsc(t)
	b.Restore(snap)
	return b
}

func TestTheHandledAlarmsSurviveARestart(t *testing.T) {
	e := newEsc(t)
	intrusion(e, t0, "x")
	ackBy(e, t0.Add(time.Minute), "x", "guard")
	e = restart(t, e)
	mustBeQuiet(t, sendsOf(sirenFails(e, t0.Add(2*time.Minute), "y", "x"), "y"), "x")
}

// A hub restarted during the alarm reports the siren again, under a new incident:
// the phones are not rung a second time. Another siren, or another alarm, is told.
func TestASirenIsReportedOncePerAlarmAcrossARestart(t *testing.T) {
	e := newEsc(t)
	intrusion(e, t0, "x")
	if len(sends(sirenFails(e, t0.Add(time.Minute), "y1", "x"))) == 0 {
		t.Fatal("the first report is told")
	}
	e = restart(t, e)
	out := sirenFails(e, t0.Add(2*time.Minute), "y2", "x")
	if _, ok := e.s.Incidents["y2/siren_failed"]; ok || len(sendsOf(out, "y2")) != 0 {
		t.Fatalf("the same siren in the same alarm again: %+v", out)
	}
	if len(sends(sirenFails(e, t0.Add(3*time.Minute), "y3", "x", "dev_siren2"))) == 0 {
		t.Fatal("another siren is told")
	}
	if len(sends(sirenFails(e, t0.Add(4*time.Minute), "y4", "x9"))) == 0 {
		t.Fatal("the same siren in another alarm is told")
	}
}

func TestHandledIsForgottenAfterADay(t *testing.T) {
	e := newEsc(t)
	ackBy(e, t0, "x", "owner") // a late "1": its escalation was already over
	sirenFails(e, t0, "y1", "x")
	day := t0.Add(24 * time.Hour)
	out := sirenFails(e, day, "y2", "x") // the same siren is told again, loud
	if !slices.Contains(sends(out), "guard/call/1") || len(e.s.Handled) != 0 || len(e.s.SirenReported) != 1 {
		t.Fatalf("%v %+v %+v", sends(out), e.s.Handled, e.s.SirenReported)
	}
}

// A state saved by the hub before quiet alerts existed: an intrusion in progress.
const oldSnapshot = `{"next_key":1,"uplink_down":false,"incidents":{"x/intrusion":{"id":"x","kind":"intrusion",
"urgency":"critical","devices":["dev_back"],"percent":0,"raised_at":"2026-10-17T21:04:00Z","step":1,
"next_at":"2026-10-17T21:06:00Z","wave_contacts":["deputy"],"rounds":{"deputy":{"step":0,
"started":"2026-10-17T21:04:00Z","sent":["app"],"net_ok":false,"net_held":false,
"net_deadline":"2026-10-17T21:04:10Z","gsm_due":"0001-01-01T00:00:00Z"}},
"deliveries":{"1":{"ref":"REQ1","contact":"deputy","channel":"app","step":0,"status":""}}}},
"recent":{"deputy":["2026-10-17T21:04:00Z"]}}`

func TestAnOldSnapshotStillLoads(t *testing.T) {
	var snap Snapshot
	if err := json.Unmarshal([]byte(oldSnapshot), &snap); err != nil {
		t.Fatal(err)
	}
	e := newEsc(t)
	e.Restore(snap)
	want(t, "the round goes on", sends(tick(e, t0.Add(10*time.Second))), "deputy/sms/1")
	if outcomeOf(t, ackBy(e, t0.Add(time.Minute), "x", "owner"), "x") != OutcomeAcknowledged {
		t.Fatal("the old incident is acknowledged")
	}
	mustBeQuiet(t, sendsOf(sirenFails(e, t0.Add(2*time.Minute), "y", "x"), "y"), "x")
}
