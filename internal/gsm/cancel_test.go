package gsm

import (
	"strings"
	"testing"
	"time"
)

// noNetwork checks the only line, which answers without network.
func noNetwork(g *Gateway, now time.Time) {
	g.Handle(Tick{Now: now})
	g.Handle(Probe{Now: now, Line: "line1", Responding: true, Registered: false})
}

// network has the line answer a check with its network back.
func network(g *Gateway, now time.Time) []Action {
	return g.Handle(Probe{Now: now, Line: "line1", Responding: true, Registered: true, Signal: 70})
}

func TestARequestTooOldOnArrivalIsGivenUpAtOnce(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	// Asked at 21:00, held by the broker while the gateway was down, arrived at 21:11.
	r := req("r1", "owner", ChannelSMS, PriorityIntrusion, t0.Add(11*time.Minute))
	r.Asked = t0
	out := g.Handle(r)
	onlyReport(t, out, "r1", StatusFailed, ReasonTimeout)
	if len(of[Execute](out)) != 0 || g.Queued() != 0 {
		t.Fatalf("a request older than its maximum age must not go: %+v", out)
	}
}

func TestTheAgeCountsFromWhenTheRequestWasAsked(t *testing.T) {
	g := newGW(t, 1)
	noNetwork(g, t0)
	r := req("r1", "owner", ChannelSMS, PriorityIntrusion, t0.Add(8*time.Minute))
	r.Asked = t0
	onlyReport(t, g.Handle(r), "r1", StatusQueued, "")
	if w := g.NextWake(); w.After(t0.Add(10 * time.Minute)) {
		t.Fatalf("next wake %v", w)
	}
	onlyReport(t, g.Handle(Tick{Now: t0.Add(10 * time.Minute)}), "r1", StatusFailed, ReasonTimeout)
}

func TestARequestFromTheFutureCountsFromItsArrival(t *testing.T) {
	g := newGW(t, 1)
	noNetwork(g, t0)
	r := req("r1", "owner", ChannelSMS, PriorityIntrusion, t0)
	r.Asked = t0.Add(time.Hour) // the clock was set back since
	g.Handle(r)
	if w := g.NextWake(); w.After(t0.Add(10 * time.Minute)) {
		t.Fatalf("next wake %v: the request must not wait an hour more", w)
	}
}

func TestNothingGoesOutAfterTheAlarmIsDisarmed(t *testing.T) {
	// No network at 21:04, disarmed at 21:05, network back at 21:08.
	g := newGW(t, 1)
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }
	noNetwork(g, at(4))
	sms := Request{Now: at(4), ID: "r1", Incident: "inc", Contact: "owner", Channel: ChannelSMS, Priority: PriorityIntrusion, Text: "ALARME intrusion"}
	call := Request{Now: at(4), ID: "r2", Incident: "inc", Contact: "guard", Channel: ChannelCall, Priority: PriorityIntrusion}
	other := Request{Now: at(4), ID: "r3", Incident: "other", Contact: "owner", Channel: ChannelSMS, Priority: PriorityOther, Text: "pile faible"}
	g.Handle(sms)
	g.Handle(call)
	g.Handle(other)
	out := g.Handle(Cancel{Now: at(5), Incident: "inc"})
	if d := of[Dropped](out); len(d) != 1 || d[0].Incident != "inc" || d[0].Jobs != 2 || len(of[Report](out)) != 0 {
		t.Fatalf("%+v", out)
	}
	ex := of[Execute](network(g, at(8)))
	if len(ex) != 1 || ex[0].Text != "pile faible" {
		t.Fatalf("only the other incident's message goes: %+v", ex)
	}
	// A request of the incident delivered late is dropped too.
	late := Request{Now: at(9), ID: "r4", Incident: "inc", Contact: "owner", Channel: ChannelSMS, Priority: PriorityIntrusion}
	if out := g.Handle(late); len(of[Dropped](out)) != 1 || len(of[Execute](out)) != 0 || g.Queued() != 1 {
		t.Fatalf("%+v", out)
	}
	// Test messages have no incident: an empty one cancels nothing.
	if out := g.Handle(Cancel{Now: at(9), Incident: ""}); len(out) != 0 || g.Queued() != 1 {
		t.Fatalf("%+v", out)
	}
}

func TestAMessageBeingSentWhenTheIncidentEndsIsNotRetried(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	ex := of[Execute](g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0)))[0]
	if out := g.Handle(Cancel{Now: t0.Add(time.Second), Incident: "inc-r1"}); len(of[Dropped](out)) != 0 {
		t.Fatalf("the line still sends it: %+v", out)
	}
	out := g.Handle(Outcome{Now: t0.Add(2 * time.Second), Job: ex.Job, Line: "line1", Reason: ReasonNoNetwork})
	if len(of[Report](out)) != 0 || g.Queued() != 0 {
		t.Fatalf("a failed message of an ended incident is dropped, not retried: %+v", out)
	}

	// One that goes anyway is reported as usual.
	network(g, t0.Add(time.Minute))
	ex = of[Execute](g.Handle(req("r2", "owner", ChannelSMS, PriorityIntrusion, t0.Add(time.Minute))))[0]
	g.Handle(Cancel{Now: t0.Add(61 * time.Second), Incident: "inc-r2"})
	onlyReport(t, g.Handle(Outcome{Now: t0.Add(62 * time.Second), Job: ex.Job, Line: "line1", OK: true}), "r2", StatusSent, "")
}

func TestAContactAnswering1DropsTheIncidentsWaitingMessages(t *testing.T) {
	g := newGW(t, 1)
	up(t, g, t0)
	a := Request{Now: t0, ID: "a", Incident: "inc", Contact: "owner", Channel: ChannelSMS, Priority: PriorityIntrusion}
	b := Request{Now: t0, ID: "b", Incident: "inc", Contact: "guard", Channel: ChannelCall, Priority: PriorityIntrusion}
	ex := of[Execute](g.Handle(a))[0]
	g.Handle(b) // waits for the line
	ex = of[Execute](g.Handle(Outcome{Now: t0.Add(time.Second), Job: ex.Job, Line: "line1", OK: true}))[0]
	g.Handle(Outcome{Now: t0.Add(2 * time.Second), Job: ex.Job, Line: "line1", Reason: ReasonNoNetwork}) // b waits to be retried
	out := g.Handle(Incoming{Now: t0.Add(3 * time.Second), Line: "line1", From: "+22890123456", Text: "1"})
	if len(of[Acknowledge](out)) != 1 || len(of[Dropped](out)) != 1 || g.Queued() != 0 {
		t.Fatalf("the call to the guard must not ring any more: %+v", out)
	}
}

// A silent siren is an incident of its own that names its alarm: the alarm's end
// (a disarm, or an acknowledgement) does not drop the SMS that says the shop was
// silent.
func TestASilentSirensSMSOutlivesItsAlarm(t *testing.T) {
	g := newGW(t, 1)
	noNetwork(g, t0)
	g.Handle(Request{Now: t0, ID: "r1", Incident: "alarm", Contact: "owner", Channel: ChannelSMS, Priority: PriorityIntrusion, Text: "ALARME intrusion"})
	g.Handle(Request{Now: t0, ID: "r2", Incident: "siren", Contact: "guard", Channel: ChannelSMS, Priority: PriorityIntrusion,
		Text: "Boutique: la sirene n'a pas sonne a 21:00. L'alarme est deja prise en charge."})
	if d := of[Dropped](g.Handle(Cancel{Now: t0.Add(time.Second), Incident: "alarm"})); len(d) != 1 || d[0].Jobs != 1 {
		t.Fatalf("%+v", d)
	}
	ex := of[Execute](network(g, t0.Add(2*time.Second)))
	if len(ex) != 1 || !strings.Contains(ex[0].Text, "sirene") {
		t.Fatalf("the siren's SMS still goes: %+v", ex)
	}
}
