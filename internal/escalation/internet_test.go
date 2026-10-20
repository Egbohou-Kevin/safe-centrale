package escalation

import (
	"testing"
	"time"
)

// --- the uplink --------------------------------------------------------------------------

func TestWithoutUplinkTheGSMChannelsTakeOverAtOnce(t *testing.T) {
	e := newEsc(t)
	e.Handle(Uplink{Now: t0, Up: false})
	got := sends(raise(e, t0.Add(time.Second), KindIntrusion))
	want(t, "no internet", got, "deputy/sms/1", "guard/sms/1", "guard/call/1")

	// When the uplink returns, the internet message that could not go is sent.
	back := sends(e.Handle(Uplink{Now: t0.Add(30 * time.Second), Up: true}))
	want(t, "uplink back", back, "deputy/app/1")
	if e.UplinkDown() {
		t.Fatal("the uplink is back")
	}
}

func TestAContactWithoutInternetIsTolDByGSMEvenWithUplink(t *testing.T) {
	e := newEsc(t, func(c *Config) {
		c.Contacts = []Contact{{ID: "owner", Role: RoleOwner, Channels: []Channel{ChannelSMS, ChannelCall}}}
	})
	want(t, "gsm only", sends(raise(e, t0, KindIntrusion)), "owner/sms/1", "owner/call/1")
}

func TestAnInternetFailureIsRetriedWhenTheUplinkComesBack(t *testing.T) {
	e := newEsc(t)
	first := raise(e, t0, KindIntrusion)
	e.Handle(Result{Now: t0.Add(time.Second), Key: keyOf(first, "deputy", ChannelApp), Status: ResultFailed})
	e.Handle(Uplink{Now: t0.Add(2 * time.Second), Up: false})
	want(t, "uplink back", sends(e.Handle(Uplink{Now: t0.Add(20 * time.Second), Up: true})), "deputy/app/1")
}

// --- alerts that wait for the internet ---------------------------------------------------

func TestSummariesUseTheInternetOnly(t *testing.T) {
	e := newEsc(t)
	got := sends(e.Handle(Raise{Now: t0, Incident: "sum1", Kind: KindBatteryLow, Urgency: UrgencyLow, Devices: []string{"dev_a"}}))
	want(t, "summary", got, "owner/app/1")
	if got := sends(tick(e, t0.Add(24*time.Hour))); len(got) != 0 {
		t.Fatalf("a summary never goes by SMS: %v", got)
	}
	if c := concluded(tick(e, t0.Add(24*time.Hour))); len(c) != 0 {
		t.Logf("%v", c)
	}
}

func TestASummaryWaitsForTheUplink(t *testing.T) {
	e := newEsc(t)
	e.Handle(Uplink{Now: t0, Up: false})
	if got := sends(e.Handle(Raise{Now: t0, Incident: "s", Kind: KindDeviceOffline, Urgency: UrgencyLow})); len(got) != 0 {
		t.Fatalf("no uplink, no SMS: %v", got)
	}
	back := e.Handle(Uplink{Now: t0.Add(time.Hour), Up: true})
	want(t, "uplink back", sends(back), "owner/app/1")
	if c := concluded(back); len(c) != 1 || e.Active() != 0 {
		t.Fatalf("the held summary was the last thing owed: the escalation must end, got %+v", back)
	}
}

func TestAConfirmationThatStopsTheLastTimerEndsTheEscalation(t *testing.T) {
	e := newEsc(t, func(c *Config) {
		c.Contacts = []Contact{{ID: "owner", Role: RoleOwner, Channels: []Channel{ChannelApp, ChannelSMS}}}
	})
	out := raise(e, t0, KindIntrusion)
	for _, at := range []time.Duration{2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 25 * time.Minute, 35 * time.Minute} {
		out = tick(e, t0.Add(at))
	}
	// The cloud confirms the last reminder before its deadline: nothing is left.
	done := e.Handle(Result{Now: t0.Add(35*time.Minute + time.Second), Key: keyOf(out, "owner", ChannelApp), Status: ResultDelivered})
	if c := concluded(done); len(c) != 1 || c[0].Outcome != OutcomeUnanswered {
		t.Fatalf("the escalation must end once its last timer is stopped: %+v", done)
	}
	if e.Active() != 0 || !e.NextWake().IsZero() {
		t.Fatal("an escalation with nothing left to do must not linger")
	}
}

func TestAFailedMainsRestoredStillWaitsItsHourForTheGSM(t *testing.T) {
	e := newEsc(t)
	out := e.Handle(Raise{Now: t0, Incident: "m", Kind: KindMainsRestored, Urgency: UrgencyLow, Percent: 55})
	failed := e.Handle(Result{Now: t0.Add(time.Second), Key: keyOf(out, "deputy", ChannelApp), Status: ResultFailed})
	if got := sends(failed); len(got) != 0 {
		t.Fatalf("too early for the GSM: %v", got)
	}
	want(t, "after an hour", sends(tick(e, t0.Add(time.Hour))), "deputy/sms/1")
}

func TestMainsRestoredUsesTheGSMOnlyAfterALongTime(t *testing.T) {
	e := newEsc(t)
	e.Handle(Uplink{Now: t0, Up: false})
	e.Handle(Raise{Now: t0, Incident: "m", Kind: KindMainsRestored, Urgency: UrgencyLow, Percent: 55})
	if got := sends(tick(e, t0.Add(30*time.Minute))); len(got) != 0 {
		t.Fatalf("too early for the GSM: %v", got)
	}
	want(t, "after an hour", sends(tick(e, t0.Add(time.Hour))), "deputy/sms/1")
}

func TestMainsRestoredIsNotDoubledWhenTheInternetWorked(t *testing.T) {
	e := newEsc(t)
	out := e.Handle(Raise{Now: t0, Incident: "m", Kind: KindMainsRestored, Urgency: UrgencyLow, Percent: 55})
	e.Handle(Result{Now: t0.Add(time.Second), Key: keyOf(out, "deputy", ChannelApp), Status: ResultDelivered})
	if got := sends(tick(e, t0.Add(2*time.Hour))); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if e.Active() != 0 {
		t.Fatal("the alert is over once its timers are done")
	}
}
