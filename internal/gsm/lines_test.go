package gsm

import (
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
