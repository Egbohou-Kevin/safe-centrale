package gsm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

func newGW(t *testing.T, lines int, mutate ...func(*Config)) *Gateway {
	t.Helper()
	var ls []Line
	for i := 1; i <= lines; i++ {
		ls = append(ls, Line{ID: "line" + string(rune('0'+i)), Operator: "op"})
	}
	cfg := DefaultConfig(ls, map[string]string{"owner": "90 12 34 56", "guard": "+228 70 00 00 01"})
	for _, m := range mutate {
		m(&cfg)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// up checks every line and answers that it works.
func up(t *testing.T, g *Gateway, now time.Time) {
	t.Helper()
	for _, a := range g.Handle(Tick{Now: now}) {
		if c, ok := a.(CheckLine); ok {
			g.Handle(Probe{Now: now, Line: c.Line, Responding: true, Registered: true, Signal: 71, Operator: "togocom"})
		}
	}
	for _, l := range g.Lines() {
		if !l.Usable() {
			t.Fatalf("%s is not usable after its check", l.ID)
		}
	}
}

func of[T Action](actions []Action) []T {
	var out []T
	for _, a := range actions {
		if v, ok := a.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

func req(id, contact string, ch Channel, p Priority, now time.Time) Request {
	return Request{Now: now, ID: id, Incident: "inc-" + id, Contact: contact, Channel: ch, Priority: p, Text: "ALARME Boutique"}
}

func onlyReport(t *testing.T, actions []Action, ref string, status Status, reason Reason) {
	t.Helper()
	rs := of[Report](actions)
	if len(rs) != 1 || rs[0].Ref != ref || rs[0].Status != status || rs[0].Reason != reason {
		t.Fatalf("want one report %s %s %s, got %+v", ref, status, reason, actions)
	}
}

func TestAnSMSIsQueuedSentAndDelivered(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	out := g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0))
	onlyReport(t, out, "r1", StatusQueued, "")
	ex := of[Execute](out)
	if len(ex) != 1 || ex[0].Number != "+22890123456" || ex[0].Line != "line1" || ex[0].Channel != ChannelSMS || ex[0].Text != "ALARME Boutique" {
		t.Fatalf("%+v", out)
	}
	out = g.Handle(Outcome{Now: t0.Add(3 * time.Second), Job: ex[0].Job, Line: "line1", OK: true, MsgRef: "42"})
	onlyReport(t, out, "r1", StatusSent, "")
	out = g.Handle(DeliveryReport{Now: t0.Add(9 * time.Second), Line: "line1", MsgRef: "42", OK: true})
	onlyReport(t, out, "r1", StatusDelivered, "")
	if g.Queued() != 0 {
		t.Fatal("the queue must be empty")
	}
	// A delivery report nobody waits for is ignored.
	if out := g.Handle(DeliveryReport{Now: t0.Add(10 * time.Second), Line: "line1", MsgRef: "42", OK: true}); len(out) != 0 {
		t.Fatalf("%+v", out)
	}
}

func TestAFailedDeliveryIsReported(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	ex := of[Execute](g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0)))[0]
	g.Handle(Outcome{Now: t0, Job: ex.Job, Line: "line1", OK: true, MsgRef: "7"})
	onlyReport(t, g.Handle(DeliveryReport{Now: t0, Line: "line1", MsgRef: "7"}), "r1", StatusFailed, ReasonUnreachable)
}

func TestACallRingsForTheConfiguredTime(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	ex := of[Execute](g.Handle(req("r1", "guard", ChannelCall, PriorityIntrusion, t0)))
	if len(ex) != 1 || ex[0].Channel != ChannelCall || ex[0].RingFor != 20*time.Second || ex[0].Number != "+22870000001" {
		t.Fatalf("%+v", ex)
	}
	// The line has its ringing time on top of the usual time to answer.
	if got := g.NextWake(); !got.Equal(t0.Add(110 * time.Second)) {
		t.Fatalf("next wake %v", got)
	}
}

func TestARequestDeliveredTwiceSendsOnce(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0))
	if out := g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0)); len(out) != 0 {
		t.Fatalf("%+v", out)
	}
	if g.Queued() != 1 {
		t.Fatal("one job")
	}
}

func TestAContactWithoutANumberFailsAtOnce(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	onlyReport(t, g.Handle(req("r1", "stranger", ChannelSMS, PriorityIntrusion, t0)), "r1", StatusFailed, ReasonUnreachable)
}

func TestOtherChannelsAreNotForTheGateway(t *testing.T) {
	g := newGW(t, 1)
	if out := g.Handle(req("r1", "owner", "app", PriorityIntrusion, t0)); len(of[Report](out)) != 0 {
		t.Fatalf("%+v", out)
	}
}

func TestFireAndPanicJumpTheQueue(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	first := of[Execute](g.Handle(req("other", "owner", ChannelSMS, PriorityOther, t0)))[0]
	g.Handle(req("intrusion", "owner", ChannelSMS, PriorityIntrusion, t0.Add(time.Second)))
	g.Handle(req("fire", "owner", ChannelSMS, PriorityLifeSafety, t0.Add(2*time.Second)))
	// The SMS in progress is not interrupted; the fire goes right after it.
	out := g.Handle(Outcome{Now: t0.Add(3 * time.Second), Job: first.Job, Line: "line1", OK: true})
	ex := of[Execute](out)
	if len(ex) != 1 {
		t.Fatalf("%+v", out)
	}
	jobs := g.Snapshot().Jobs
	for _, j := range jobs {
		if j.ID == ex[0].Job && j.Ref != "fire" {
			t.Fatalf("%s went before the fire", j.Ref)
		}
	}
}

func TestAFailureTriesTheOtherLineFirst(t *testing.T) {
	g := newGW(t, 2)
	up(t, g, t0)
	ex := of[Execute](g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0)))[0]
	if ex.Line != "line1" {
		t.Fatalf("%+v", ex)
	}
	out := g.Handle(Outcome{Now: t0.Add(time.Second), Job: ex.Job, Line: "line1", Reason: ReasonNoCredit})
	if len(of[Report](out)) != 0 {
		t.Fatalf("no failure yet, another line is left: %+v", out)
	}
	again := of[Execute](out)
	if len(again) != 1 || again[0].Line != "line2" {
		t.Fatalf("%+v", out)
	}
	if g.Lines()[0].Credit != "empty" {
		t.Fatal("the operator said the SIM has no credit")
	}
	onlyReport(t, g.Handle(Outcome{Now: t0.Add(2 * time.Second), Job: again[0].Job, Line: "line2", OK: true}), "r1", StatusSent, "")
	// The line that sent last is tried first next time.
	if ex := of[Execute](g.Handle(req("r2", "owner", ChannelSMS, PriorityIntrusion, t0.Add(3*time.Second)))); ex[0].Line != "line2" {
		t.Fatalf("%+v", ex)
	}
}

func TestWithOneLineAFailureIsReportedOnceThenRetried(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	ex := of[Execute](g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0)))[0]
	out := g.Handle(Outcome{Now: t0.Add(time.Second), Job: ex.Job, Line: "line1", Reason: ReasonModemError})
	onlyReport(t, out, "r1", StatusFailed, ReasonModemError)
	if len(of[Execute](out)) != 0 {
		t.Fatal("the retry waits")
	}
	if got := g.NextWake(); !got.Equal(t0.Add(6 * time.Second)) {
		t.Fatalf("retry at %v, want 5 s after the failure", got)
	}
	retry := of[Execute](g.Handle(Tick{Now: t0.Add(6 * time.Second)}))
	if len(retry) != 1 {
		t.Fatal("the retry")
	}
	// A second failure is not reported again; the wait doubles.
	if out := g.Handle(Outcome{Now: t0.Add(7 * time.Second), Job: retry[0].Job, Line: "line1", Reason: ReasonModemError}); len(of[Report](out)) != 0 {
		t.Fatalf("%+v", out)
	}
	if got := g.NextWake(); !got.Equal(t0.Add(17 * time.Second)) {
		t.Fatalf("second retry at %v", got)
	}
	retry = of[Execute](g.Handle(Tick{Now: t0.Add(17 * time.Second)}))
	// It finally goes: the escalation learns it.
	onlyReport(t, g.Handle(Outcome{Now: t0.Add(18 * time.Second), Job: retry[0].Job, Line: "line1", OK: true}), "r1", StatusSent, "")
}

func TestTheBackoffIsCapped(t *testing.T) {
	g := newGW(t, 1)
	if g.backoff(1) != 5*time.Second || g.backoff(2) != 10*time.Second || g.backoff(20) != 2*time.Minute {
		t.Fatalf("%v %v %v", g.backoff(1), g.backoff(2), g.backoff(20))
	}
}

func TestARequestTooOldIsGivenUp(t *testing.T) {
	g := newGW(t, 1)
	g.Handle(Tick{Now: t0})
	g.Handle(Probe{Now: t0, Line: "line1", Responding: true, Registered: false}) // no network
	out := g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0))
	if len(of[Execute](out)) != 0 {
		t.Fatal("no line can send")
	}
	if got := g.NextWake(); !got.Equal(t0.Add(30 * time.Second)) {
		t.Fatalf("%v: the next check comes first", got)
	}
	// Ten minutes without network: the request is given up and reported.
	var reports []Report
	for now := t0.Add(30 * time.Second); !now.After(t0.Add(11 * time.Minute)); now = now.Add(30 * time.Second) {
		for _, a := range g.Handle(Tick{Now: now}) {
			if c, ok := a.(CheckLine); ok {
				g.Handle(Probe{Now: now, Line: c.Line, Responding: true, Registered: false})
			}
			if r, ok := a.(Report); ok {
				reports = append(reports, r)
			}
		}
	}
	if len(reports) != 1 || reports[0].Status != StatusFailed || reports[0].Reason != ReasonTimeout || g.Queued() != 0 {
		t.Fatalf("%+v", reports)
	}
}

func TestNextWakeNeverPointsToThePastWhenNothingCanBeDone(t *testing.T) {
	g := newGW(t, 1)
	g.Handle(Tick{Now: t0})
	g.Handle(Probe{Now: t0, Line: "line1"}) // not responding
	g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0))
	now := t0.Add(5 * time.Second)
	g.Handle(Tick{Now: now})
	if w := g.NextWake(); !w.After(now) {
		t.Fatalf("next wake %v at %v: the driver would spin", w, now)
	}
}

func TestALineThatDoesNotAnswerFails(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0))
	out := g.Handle(Tick{Now: t0.Add(90 * time.Second)})
	onlyReport(t, out, "r1", StatusFailed, ReasonTimeout)
}

func TestALateAnswerDoesNotDisturbTheLineNowSending(t *testing.T) {
	g := newGW(t, 2)
	up(t, g, t0)
	ex := of[Execute](g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0)))[0]
	// line1 is too slow: the job goes to line2.
	out := g.Handle(Tick{Now: t0.Add(90 * time.Second)})
	moved := of[Execute](out)
	if len(moved) != 1 || moved[0].Line != "line2" {
		t.Fatalf("%+v", out)
	}
	// line1 finally says it failed: line2 goes on.
	if out := g.Handle(Outcome{Now: t0.Add(91 * time.Second), Job: ex.Job, Line: "line1", Reason: ReasonModemError}); len(of[Execute](out)) != 0 {
		t.Fatalf("%+v", out)
	}
	onlyReport(t, g.Handle(Outcome{Now: t0.Add(92 * time.Second), Job: ex.Job, Line: "line2", OK: true}), "r1", StatusSent, "")
	// And an outcome for a job that is gone is ignored.
	if out := g.Handle(Outcome{Now: t0.Add(93 * time.Second), Job: ex.Job, Line: "line1", OK: true}); len(of[Report](out)) != 0 {
		t.Fatalf("%+v", out)
	}
}

func TestARestartKeepsTheQueue(t *testing.T) {
	a := newGW(t, 1)
	up(t, a, t0)
	ex := of[Execute](a.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0)))[0]
	a.Handle(req("r2", "owner", ChannelSMS, PriorityIntrusion, t0))
	raw, err := json.Marshal(a.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	b := newGW(t, 1)
	b.Restore(snap)
	if b.Queued() != 2 {
		t.Fatal("both requests survive")
	}
	// The SMS that was being sent goes again, after the line is checked.
	up(t, b, t0.Add(time.Minute))
	again := of[Execute](b.Handle(Tick{Now: t0.Add(time.Minute)}))
	if len(again) != 0 {
		t.Fatalf("%+v", again)
	}
	jobs := b.Snapshot().Jobs
	if jobs[0].ID != ex.Job || jobs[0].Line != "line1" {
		t.Fatalf("%+v", jobs[0])
	}
	// A request seen before the restart is still a duplicate.
	if out := b.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0.Add(time.Minute))); len(out) != 0 {
		t.Fatalf("%+v", out)
	}
}

func TestSnapshotsAreCopies(t *testing.T) {
	g := newGW(t, 1, func(c *Config) { c.TestNumber = "90000000" })
	up(t, g, t0)
	g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0))
	s := g.Snapshot()
	s.Jobs[0].Text = "changed"
	s.Seen[0] = "changed"
	if g.Snapshot().Jobs[0].Text == "changed" || g.Snapshot().Seen[0] == "changed" {
		t.Fatal("a snapshot must not share memory")
	}
	if !reflect.DeepEqual(g.Snapshot().Lines, g.Snapshot().Lines) {
		t.Fatal("stable")
	}
}

func TestDescribeNeverShowsATextOrANumber(t *testing.T) {
	for _, a := range []Action{
		Execute{Job: 1, Line: "line1", Channel: ChannelSMS, Number: "+22890123456", Text: "secret"},
		CheckLine{Line: "l"}, RestartModem{Line: "l"}, Report{Ref: "r", Status: StatusSent},
		Acknowledge{Incident: "i", Contact: "c"}, LinesChanged{}, Ignored{Line: "l", Why: "w"}, Dropped{Incident: "i", Jobs: 2}, nil,
	} {
		d := Describe(a)
		if d == "" || strings.Contains(d, "+228") || strings.Contains(d, "secret") {
			t.Errorf("%q", d)
		}
	}
}

func TestALateFailureDoesNotSendTheJobAThirdTime(t *testing.T) {
	g := newGW(t, 3)
	up(t, g, t0)
	ex := of[Execute](g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0)))[0]
	tick := g.Handle(Tick{Now: t0.Add(90 * time.Second)}) // line1 too slow
	moved := of[Execute](tick)
	if len(moved) != 1 || moved[0].Line == ex.Line {
		t.Fatalf("%+v", moved)
	}
	for _, c := range of[CheckLine](tick) { // the idle lines are checked meanwhile, and work
		g.Handle(Probe{Now: t0.Add(90 * time.Second), Line: c.Line, Responding: true, Registered: true, Signal: 50})
	}
	// line1 answers late that it failed: line2 is still sending, nobody else must.
	out := g.Handle(Outcome{Now: t0.Add(91 * time.Second), Job: ex.Job, Line: ex.Line, Reason: ReasonModemError})
	if got := of[Execute](out); len(got) != 0 {
		t.Fatalf("the SMS would go out twice at once: %+v", got)
	}
}

func TestAWaitOverWithNoLineLeftDoesNotSpinTheDriver(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	ex := of[Execute](g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0)))[0]
	g.Handle(Outcome{Now: t0.Add(time.Second), Job: ex.Job, Line: "line1", Reason: ReasonNoNetwork}) // waits 5 s
	now := t0.Add(10 * time.Second)                                                                  // the wait is over, the line still has no network
	g.Handle(Tick{Now: now})
	if w := g.NextWake(); !w.After(now) {
		t.Fatalf("next wake %v at %v: the driver would spin", w, now)
	}
}

func TestALateAnswerDoesNotFreeALineBusyWithAnotherJob(t *testing.T) {
	g := newGW(t, 2)
	up(t, g, t0)
	a := of[Execute](g.Handle(req("a", "owner", ChannelSMS, PriorityIntrusion, t0)))[0]
	now := t0.Add(90 * time.Second) // line1 too slow: a goes to line2, line1 is checked
	for _, c := range of[CheckLine](g.Handle(Tick{Now: now})) {
		g.Handle(Probe{Now: now, Line: c.Line, Responding: true, Registered: true, Signal: 50})
	}
	b := of[Execute](g.Handle(req("b", "owner", ChannelSMS, PriorityIntrusion, now.Add(time.Second))))
	if len(b) != 1 || b[0].Line != a.Line {
		t.Fatalf("b goes to the line that is free again: %+v", b)
	}
	// line1 answers late for a, while it sends b.
	g.Handle(Outcome{Now: now.Add(2 * time.Second), Job: a.Job, Line: a.Line, Reason: ReasonModemError})
	if ex := of[Execute](g.Handle(req("c", "owner", ChannelSMS, PriorityIntrusion, now.Add(3*time.Second)))); len(ex) != 0 {
		t.Fatalf("both lines are busy, c must wait: %+v", ex)
	}
}

func TestALateFailureForAWaitingJobDoesNotLengthenItsWait(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	ex := of[Execute](g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0)))[0]
	onlyReport(t, g.Handle(Tick{Now: t0.Add(90 * time.Second)}), "r1", StatusFailed, ReasonTimeout)
	g.Handle(Outcome{Now: t0.Add(91 * time.Second), Job: ex.Job, Line: "line1", Reason: ReasonModemError})
	if got := g.NextWake(); !got.Equal(t0.Add(95 * time.Second)) {
		t.Fatalf("retry at %v, want 5 s after the timeout", got)
	}
}
