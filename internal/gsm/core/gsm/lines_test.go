package gsm

import (
	"fmt"
	"testing"
	"time"
)

func TestAtStartEveryLineIsChecked(t *testing.T) {
	g := newGW(t, 2)
	if !g.NextWake().Before(t0) {
		t.Fatal("the first checks are due at once")
	}
	checks := of[CheckLine](g.Handle(Tick{Now: t0}))
	if len(checks) != 2 || checks[0].Line != "line1" || checks[1].Line != "line2" {
		t.Fatalf("%+v", checks)
	}
	out := g.Handle(Probe{Now: t0, Line: "line1", Responding: true, Registered: true, Signal: 67, Operator: "togocom"})
	ch := of[LinesChanged](out)
	if len(ch) != 1 || ch[0].Lines[0].Signal != 70 || ch[0].Lines[0].Operator != "togocom" || !ch[0].Lines[0].Responding || !ch[0].Lines[0].Registered {
		t.Fatalf("%+v", out)
	}
	// The same reading again changes nothing public.
	g.Handle(Tick{Now: t0.Add(30 * time.Second)})
	if out := g.Handle(Probe{Now: t0.Add(30 * time.Second), Line: "line1", Responding: true, Registered: true, Signal: 69}); len(of[LinesChanged](out)) != 0 {
		t.Fatalf("a signal moving inside its band must not republish: %+v", out)
	}
}

func TestAModemThatStopsAnsweringIsRestarted(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	now := t0
	var restarts []RestartModem
	for i := 0; i < 3; i++ {
		now = now.Add(30 * time.Second)
		for _, a := range g.Handle(Tick{Now: now}) {
			if _, ok := a.(CheckLine); ok {
				restarts = append(restarts, of[RestartModem](g.Handle(Probe{Now: now, Line: "line1"}))...)
			}
		}
	}
	if len(restarts) != 1 {
		t.Fatalf("restart after three silent checks, got %d", len(restarts))
	}
	if l := g.Lines()[0]; l.Responding || l.Restarts != 1 {
		t.Fatalf("%+v", l)
	}
	// The restart is over: the line is checked again at once.
	if checks := of[CheckLine](g.Handle(Restarted{Now: now.Add(time.Minute), Line: "line1"})); len(checks) != 1 {
		t.Fatal("check after the restart")
	}
}

// A modem that hangs during an alarm fails every job: the queued alerts keep its
// line busy, and no check comes. Its failed jobs count as misses: it is restarted
// after three of them, not once the queue has drained.
func TestAModemThatHangsWhileJobsKeepItBusyIsRestarted(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	now := t0
	var ex []Execute
	for i := range 6 {
		ex = append(ex, of[Execute](g.Handle(req(fmt.Sprintf("r%d", i), "owner", ChannelSMS, PriorityIntrusion, now)))...)
	}
	var restarts []RestartModem
	for range 3 {
		if len(ex) != 1 {
			t.Fatalf("one job on the line: %+v", ex)
		}
		now = now.Add(10 * time.Second)
		out := g.Handle(Outcome{Now: now, Job: ex[0].Job, Line: "line1", Reason: ReasonTimeout})
		restarts = append(restarts, of[RestartModem](out)...)
		ex = of[Execute](out) // the next job takes the line at once: no check comes
	}
	if len(restarts) != 1 {
		t.Fatalf("restart after three jobs timed out, got %d", len(restarts))
	}
	if l := g.Lines()[0]; l.Responding || l.Restarts != 1 {
		t.Fatalf("a modem being restarted does not respond: %+v", l)
	}
}

func TestACheckWithoutAnswerCountsAsSilent(t *testing.T) {
	g := newGW(t, 1, func(c *Config) { c.MissesToRestart = 1 })
	g.Handle(Tick{Now: t0})
	if out := g.Handle(Tick{Now: t0.Add(30 * time.Second)}); len(of[RestartModem](out)) != 1 {
		t.Fatalf("%+v", out)
	}
	// A restart that never says it is over ends anyway.
	if out := g.Handle(Tick{Now: t0.Add(4 * time.Minute)}); len(of[CheckLine](out)) != 1 {
		t.Fatalf("%+v", out)
	}
}

func TestAContactAnswering1AcknowledgesTheLastAlert(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	ex := of[Execute](g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0)))[0]
	g.Handle(Outcome{Now: t0, Job: ex.Job, Line: "line1", OK: true})

	out := g.Handle(Incoming{Now: t0.Add(time.Minute), Line: "line1", From: "+22890123456", Text: " 1\n"})
	acks := of[Acknowledge](out)
	if len(acks) != 1 || acks[0].Incident != "inc-r1" || acks[0].Contact != "owner" {
		t.Fatalf("%+v", out)
	}
	for name, in := range map[string]Incoming{
		"unknown number": {From: "+22899999999", Text: "1"},
		"unknown text":   {From: "90123456", Text: "STOP"},
		"bad number":     {From: "operator", Text: "1"},
		"other contact":  {From: "70000001", Text: "1"}, // the guard was told nothing
	} {
		in.Now, in.Line = t0.Add(time.Minute), "line1"
		if out := g.Handle(in); len(of[Acknowledge](out)) != 0 || len(of[Ignored](out)) != 1 {
			t.Errorf("%s: %+v", name, out)
		}
	}
	// Too late: a day later, "1" acknowledges nothing.
	if out := g.Handle(Incoming{Now: t0.Add(25 * time.Hour), Line: "line1", From: "90123456", Text: "1"}); len(of[Acknowledge](out)) != 0 {
		t.Fatalf("%+v", out)
	}
}

// A tamper, then the intrusion two minutes later: nothing in the texts says which a
// "1" answers. It ends the less urgent one, the tamper: ending the intrusion for
// everyone because of a reply meant for the tamper would stop its waves. A second "1"
// ends the intrusion. Alerts told far apart leave the last one to the "1".
func TestAnAmbiguous1EndsTheLessUrgentAlert(t *testing.T) {
	a := newAckRig(t)
	sms := func(id string, p Priority, at time.Time) { a.sms(req(id, "owner", ChannelSMS, p, at)) }
	one := a.one
	sms("tamper", PriorityOther, t0)
	sms("intrusion", PriorityIntrusion, t0.Add(2*time.Minute))
	if got := one(t0.Add(2*time.Minute + 10*time.Second)); got != "inc-tamper" {
		t.Fatalf("the 1 ended %s, want the tamper", got)
	}
	if got := one(t0.Add(3 * time.Minute)); got != "inc-intrusion" {
		t.Fatalf("the second 1 ended %s, want the intrusion", got)
	}

	// An intrusion, then a fire: the intrusion first.
	sms("intrusion2", PriorityIntrusion, t0.Add(time.Hour))
	sms("fire", PriorityLifeSafety, t0.Add(time.Hour+time.Minute))
	if got := one(t0.Add(time.Hour + 2*time.Minute)); got != "inc-intrusion2" {
		t.Fatalf("the 1 ended %s, want the intrusion", got)
	}

	// An alert of this morning does not make tonight's intrusion ambiguous.
	sms("auth", PriorityOther, t0.Add(5*time.Hour))
	sms("intrusion3", PriorityIntrusion, t0.Add(12*time.Hour))
	if got := one(t0.Add(12*time.Hour + time.Minute)); got != "inc-intrusion3" {
		t.Fatalf("the 1 ended %s, want the intrusion told last", got)
	}
}

// ackRig is a gateway with one line, whose owner is told SMS (sms) and answers "1"
// (one, which returns the incident it ended).
type ackRig struct {
	t *testing.T
	g *Gateway
}

func newAckRig(t *testing.T) *ackRig {
	g := newGW(t, 1)
	up(t, g, t0)
	return &ackRig{t: t, g: g}
}

func (a *ackRig) sms(r Request) {
	a.t.Helper()
	up(a.t, a.g, r.Now)
	ex := of[Execute](a.g.Handle(r))[0]
	a.g.Handle(Outcome{Now: r.Now, Job: ex.Job, Line: "line1", OK: true})
}

func (a *ackRig) one(at time.Time) string {
	a.t.Helper()
	out := a.g.Handle(Incoming{Now: at, Line: "line1", From: "90123456", Text: "1"})
	for _, c := range of[CheckLine](out) {
		a.g.Handle(Probe{Now: at, Line: c.Line, Responding: true, Registered: true})
	}
	acks := of[Acknowledge](out)
	if len(acks) != 1 {
		a.t.Fatalf("one acknowledgement, got %+v", acks)
	}
	return acks[0].Incident
}

// The silent siren's alert goes to the same people as its alarm, a few seconds after
// it, at the same queue priority. A "1" then ends the siren's alert, which ends only
// it: ending the intrusion would stop its waves and reminders for everyone.
func TestAnAmbiguous1EndsTheSilentSirenBeforeItsAlarm(t *testing.T) {
	a := newAckRig(t)
	a.sms(req("intrusion", "owner", ChannelSMS, PriorityIntrusion, t0))
	siren := req("siren", "owner", ChannelSMS, PriorityIntrusion, t0.Add(30*time.Second))
	siren.Secondary = true
	a.sms(siren)
	if got := a.one(t0.Add(40 * time.Second)); got != "inc-siren" {
		t.Fatalf("the 1 ended %s, want the silent siren's alert", got)
	}
	if got := a.one(t0.Add(time.Minute)); got != "inc-intrusion" {
		t.Fatalf("the second 1 ended %s, want the intrusion", got)
	}
}

// An incident still reminded is as recent as its last message, not its first: a fire
// raised 20 minutes into an intrusion is told within 10 minutes of the intrusion's
// reminders, and an ambiguous "1" ends the intrusion, not the fire.
func TestAReminderKeepsItsIncidentAmbiguous(t *testing.T) {
	a := newAckRig(t)
	told := func(incident string, p Priority, minutes ...int) {
		for _, m := range minutes {
			r := req(fmt.Sprintf("%s-%d", incident, m), "owner", ChannelSMS, p, t0.Add(time.Duration(m)*time.Minute))
			r.Incident = "inc-" + incident
			a.sms(r)
		}
	}
	told("intrusion", PriorityIntrusion, 0, 2, 15)
	told("fire", PriorityLifeSafety, 20)
	told("intrusion", PriorityIntrusion, 25)
	told("fire", PriorityLifeSafety, 30)
	told("intrusion", PriorityIntrusion, 35)
	told("fire", PriorityLifeSafety, 40)
	if got := a.one(t0.Add(41 * time.Minute)); got != "inc-intrusion" {
		t.Fatalf("the 1 ended %s, want the intrusion reminded 6 minutes before", got)
	}
}

// A "1" held by the network (the hub's SIM off the air) or stored while the gateway
// was down is read late: written long before the alert it would now answer was sent,
// it cannot be meant for it, and ends nothing. The service centre's clock may be a
// few minutes off: a "1" stamped a little before the alert still counts.
func TestA1WrittenBeforeTheAlertEndsNothing(t *testing.T) {
	a := newAckRig(t)
	a.sms(req("intrusion", "owner", ChannelSMS, PriorityIntrusion, t0.Add(time.Hour)))
	in := Incoming{Now: t0.Add(time.Hour + time.Minute), Line: "line1", From: "90123456", Text: "1", Sent: t0}
	if out := a.g.Handle(in); len(of[Acknowledge](out)) != 0 || len(of[Ignored](out)) != 1 {
		t.Fatalf("a 1 written an hour before the alert: %+v", out)
	}
	in.Sent = t0.Add(time.Hour - 3*time.Minute)
	if out := a.g.Handle(in); len(of[Acknowledge](out)) != 1 {
		t.Fatalf("a 1 from a service centre a few minutes behind: %+v", out)
	}
}

func TestTheWeeklyTestSMS(t *testing.T) {
	g := newGW(t, 1, func(c *Config) { c.TestNumber = "+228 90 00 00 00" })
	up(t, g, t0)
	week := 7 * 24 * time.Hour
	if g.Snapshot().NextTest != t0.Add(week) {
		t.Fatalf("%v", g.Snapshot().NextTest)
	}
	ex := of[Execute](g.Handle(Tick{Now: t0.Add(week)}))
	var test Execute
	for _, e := range ex {
		if e.Number == "+22890000000" {
			test = e
		}
	}
	if test.Job == 0 {
		t.Fatalf("%+v", ex)
	}
	out := g.Handle(Outcome{Now: t0.Add(week), Job: test.Job, Line: "line1", OK: true})
	if len(of[Report](out)) != 0 {
		t.Fatal("a test is not an alert: nothing is reported to the escalation")
	}
	if l := g.Lines()[0]; l.LastTestOK == nil || !*l.LastTestOK || !l.LastTest.Equal(t0.Add(week)) {
		t.Fatalf("%+v", l)
	}

	// Next week the SIM is suspended: the test fails and is not retried.
	ex = of[Execute](g.Handle(Tick{Now: t0.Add(2 * week)}))
	for _, e := range ex {
		if e.Number == "+22890000000" {
			g.Handle(Outcome{Now: t0.Add(2 * week), Job: e.Job, Line: "line1", Reason: ReasonNoCredit})
		}
	}
	if l := g.Lines()[0]; l.LastTestOK == nil || *l.LastTestOK || g.Queued() != 0 {
		t.Fatalf("%+v, %d queued", l, g.Queued())
	}
}

func TestTwoContactsSharingAPhoneAcknowledgeTheOneTold(t *testing.T) {
	g := newGW(t, 1, func(c *Config) {
		c.Contacts = map[string]string{"a_owner": "90 12 34 56", "z_manager": "+228 90 12 34 56"}
		c.CheckEvery = 24 * time.Hour // no check in the way
	})
	up(t, g, t0)
	now := t0
	for _, contact := range []string{"a_owner", "z_manager"} {
		r := req("r-"+contact, contact, ChannelSMS, PriorityIntrusion, now)
		ex := of[Execute](g.Handle(r))[0]
		g.Handle(Outcome{Now: now, Job: ex.Job, Line: "line1", OK: true})
		acks := of[Acknowledge](g.Handle(Incoming{Now: now.Add(time.Minute), Line: "line1", From: "90123456", Text: "1"}))
		if len(acks) != 1 || acks[0].Contact != contact || acks[0].Incident != r.Incident {
			t.Fatalf("%s told last: %+v", contact, acks)
		}
		now = now.Add(time.Hour)
	}
}
