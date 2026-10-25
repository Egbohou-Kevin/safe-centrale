package escalation

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 17, 21, 4, 0, 0, time.UTC)

func contacts() []Contact {
	return []Contact{
		{ID: "owner", Name: "Owner", Role: RoleOwner, Channels: []Channel{ChannelApp, ChannelSMS, ChannelCall}},
		{ID: "deputy", Name: "Deputy", Role: RoleDeputy, Channels: []Channel{ChannelApp, ChannelSMS}},
		{ID: "guard", Name: "Guard", Role: RoleGuard, Channels: []Channel{ChannelSMS, ChannelCall}},
		{ID: "manager", Name: "Manager", Role: RoleManager, Channels: []Channel{ChannelWhatsApp, ChannelSMS}},
		{ID: "other", Name: "Other", Role: RoleOther, Channels: []Channel{ChannelSMS}},
	}
}

func newEsc(t *testing.T, mutate ...func(*Config)) *Escalation {
	t.Helper()
	cfg := DefaultConfig("Boutique Lom\u00e9", contacts())
	cfg.DeviceNames = map[string]string{"dev_back": "Porte arri\u00e8re"}
	for _, m := range mutate {
		m(&cfg)
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// sends lists the messages of a batch of actions as "contact/channel/wave".
func sends(actions []Action) []string {
	var out []string
	for _, a := range actions {
		if s, ok := a.(Send); ok {
			out = append(out, fmt.Sprintf("%s/%s/%d", s.Contact, s.Channel, s.Wave))
		}
	}
	return out
}

func sortedSends(actions []Action) []string {
	s := sends(actions)
	slices.Sort(s)
	return s
}

func want(t *testing.T, what string, got []string, wanted ...string) {
	t.Helper()
	slices.Sort(wanted)
	g := slices.Clone(got)
	slices.Sort(g)
	if !slices.Equal(g, wanted) {
		t.Fatalf("%s:\n got  %v\n want %v", what, g, wanted)
	}
}

func raise(e *Escalation, at time.Time, kind AlertKind, devices ...string) []Action {
	return e.Handle(Raise{Now: at, Incident: "inc1", Kind: kind, Urgency: UrgencyCritical, Devices: devices})
}

func tick(e *Escalation, at time.Time) []Action { return e.Handle(Tick{Now: at}) }

func concluded(actions []Action) []Concluded {
	var out []Concluded
	for _, a := range actions {
		if c, ok := a.(Concluded); ok {
			out = append(out, c)
		}
	}
	return out
}

func keyOf(actions []Action, contact string, ch Channel) uint64 {
	for _, a := range actions {
		if s, ok := a.(Send); ok && s.Contact == contact && s.Channel == ch {
			return s.Key
		}
	}
	return 0
}

// --- intrusion: the waves ----------------------------------------------------------------

// A role may be held by two contacts (a couple owning the shop, two deputies): each
// is told what the role is told, not only the first one listed.
func TestEveryContactOfARoleIsTold(t *testing.T) {
	two := func(c *Config) {
		c.Contacts = []Contact{
			{ID: "o1", Name: "O1", Role: RoleOwner, Channels: []Channel{ChannelApp}},
			{ID: "o2", Name: "O2", Role: RoleOwner, Channels: []Channel{ChannelApp}},
			{ID: "d1", Name: "D1", Role: RoleDeputy, Channels: []Channel{ChannelApp}},
			{ID: "d2", Name: "D2", Role: RoleDeputy, Channels: []Channel{ChannelApp}},
		}
	}
	e := newEsc(t, two)
	want(t, "wave 1", sends(raise(e, t0, KindIntrusion)), "d1/app/1", "d2/app/1")
	want(t, "wave 2", sends(tick(e, t0.Add(2*time.Minute))), "o1/app/2", "o2/app/2", "d1/app/2", "d2/app/2")

	e = newEsc(t, two)
	want(t, "tamper", sends(raise(e, t0, KindTamper)), "d1/app/1", "d2/app/1")
	e = newEsc(t, two)
	want(t, "the owner's summary", sends(raise(e, t0, KindBatteryLow)), "o1/app/1", "o2/app/1")
}

func TestIntrusionFollowsTheWaves(t *testing.T) {
	e := newEsc(t)
	// 0: the deputy (first person) and the guard. The guard has no internet: GSM at once.
	first := raise(e, t0, KindIntrusion, "dev_back")
	want(t, "wave 1", sends(first), "deputy/app/1", "guard/sms/1", "guard/call/1")
	if e.NextWake() != t0.Add(10*time.Second) {
		t.Fatalf("next wake %v: the internet confirmation is the first deadline", e.NextWake())
	}

	// +10 s: the cloud did not confirm, the deputy gets the GSM channels.
	want(t, "internet not confirmed", sends(tick(e, t0.Add(10*time.Second))), "deputy/sms/1")
	if got := sends(tick(e, t0.Add(time.Minute))); len(got) != 0 {
		t.Fatalf("nothing is left to double: %v", got)
	}

	// +2 min: the owner is told, the guard and the deputy again.
	want(t, "wave 2", sends(tick(e, t0.Add(2*time.Minute))), "owner/app/2", "guard/sms/2", "guard/call/2", "deputy/app/2")

	// Nobody confirmed the owner's and the deputy's messages: the GSM channels follow.
	want(t, "wave 2 not confirmed", sends(tick(e, t0.Add(2*time.Minute+10*time.Second))),
		"owner/sms/2", "owner/call/2", "deputy/sms/2")

	// +5 min: the manager and the others.
	want(t, "wave 3", sends(tick(e, t0.Add(5*time.Minute))), "manager/whatsapp/3", "other/sms/3")
	want(t, "wave 3 not confirmed", sends(tick(e, t0.Add(5*time.Minute+10*time.Second))), "manager/sms/3")

	// Then the reminders, every 10 minutes: internet and SMS to everyone.
	want(t, "reminder 1", sends(tick(e, t0.Add(15*time.Minute))),
		"owner/app/4", "owner/sms/4", "deputy/app/4", "deputy/sms/4", "guard/sms/4", "guard/call/4",
		"manager/whatsapp/4", "manager/sms/4", "other/sms/4")
	if got := sends(tick(e, t0.Add(25*time.Minute))); len(got) == 0 {
		t.Fatal("reminder 2 missing")
	}
	if got := sends(tick(e, t0.Add(35*time.Minute))); len(got) == 0 {
		t.Fatal("reminder 3 missing")
	}
	if len(e.s.Incidents) == 0 {
		t.Fatal("the last reminders still wait for their confirmations")
	}

	// Nobody answered: the incident ends as unanswered once the timers are done.
	end := tick(e, t0.Add(40*time.Minute))
	if c := concluded(end); len(c) != 1 || c[0].Outcome != OutcomeUnanswered || c[0].Kind != KindIntrusion {
		t.Fatalf("%+v", end)
	}
	if len(e.s.Incidents) != 0 || !e.NextWake().IsZero() {
		t.Fatal("nothing must remain once the escalation is over")
	}
	if got := sends(tick(e, t0.Add(2*time.Hour))); len(got) != 0 {
		t.Fatalf("messages after the end: %v", got)
	}
}

func TestTheCloudConfirmationHoldsTheGSMBack(t *testing.T) {
	e := newEsc(t)
	first := raise(e, t0, KindIntrusion)
	k := keyOf(first, "deputy", ChannelApp)
	// The cloud took the message: no GSM at 10 s...
	e.Handle(Result{Now: t0.Add(2 * time.Second), Key: k, Status: ResultSent})
	if got := sends(tick(e, t0.Add(10*time.Second))); len(got) != 0 {
		t.Fatalf("the GSM must wait: %v", got)
	}
	// ...but the unanswered message is doubled by SMS at 60 s.
	want(t, "doubling", sends(tick(e, t0.Add(time.Minute))), "deputy/sms/1")
}

func TestAnInternetFailureGoesToGSMAtOnce(t *testing.T) {
	e := newEsc(t)
	first := raise(e, t0, KindIntrusion)
	k := keyOf(first, "deputy", ChannelApp)
	got := sends(e.Handle(Result{Now: t0.Add(time.Second), Key: k, Status: ResultFailed}))
	want(t, "internet failed", got, "deputy/sms/1")
}

func TestAnUnreachableWaveDoesNotDelayTheNext(t *testing.T) {
	e := newEsc(t, func(c *Config) {
		c.Contacts = []Contact{
			{ID: "owner", Role: RoleOwner, Channels: []Channel{ChannelSMS}},
			{ID: "manager", Role: RoleManager, Channels: []Channel{ChannelSMS}},
		}
	})
	first := raise(e, t0, KindIntrusion)
	want(t, "wave 1", sends(first), "owner/sms/1")
	// The SMS fails (no credit): the next wave comes now, not at +2 min.
	k := keyOf(first, "owner", ChannelSMS)
	got := sends(e.Handle(Result{Now: t0.Add(3 * time.Second), Key: k, Status: ResultFailed}))
	want(t, "next wave at once", got, "owner/sms/2")
}

func TestAFailedWaveWithOtherChannelsLeftWaits(t *testing.T) {
	e := newEsc(t)
	first := raise(e, t0, KindIntrusion)
	// The guard's SMS fails but its call is not known to have failed: no hurry.
	k := keyOf(first, "guard", ChannelSMS)
	got := sends(e.Handle(Result{Now: t0.Add(time.Second), Key: k, Status: ResultFailed}))
	if len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if e.NextWake().After(t0.Add(10 * time.Second)) {
		t.Fatal("the deputy's deadline must still be pending")
	}
}

func TestAWaveNobodyIsLeftForIsSkippedAtOnce(t *testing.T) {
	plans := DefaultPlans()
	plans[KindIntrusion] = Plan{Waves: []Wave{
		{To: []Audience{AudienceGuard}},
		{After: 5 * time.Minute, To: []Audience{AudienceOwner}},
	}}
	e := newEsc(t, func(c *Config) {
		c.Contacts = []Contact{{ID: "owner", Role: RoleOwner, Channels: []Channel{ChannelApp}}}
		c.Plans = plans
	})
	// There is no guard: the second wave does not wait its five minutes.
	want(t, "skipped wave", sends(raise(e, t0, KindIntrusion)), "owner/app/2")
}

func TestASiteWithNoContactEndsAtOnce(t *testing.T) {
	e := newEsc(t, func(c *Config) { c.Contacts = nil })
	out := raise(e, t0, KindIntrusion)
	if c := concluded(out); len(sends(out)) != 0 || len(c) != 1 || c[0].Outcome != OutcomeUnanswered {
		t.Fatalf("%+v", out)
	}
}

// --- acknowledgement, closing ------------------------------------------------------------

func TestAnAcknowledgementEndsTheEscalation(t *testing.T) {
	e := newEsc(t)
	raise(e, t0, KindIntrusion)
	out := e.Handle(Ack{Now: t0.Add(time.Minute), Incident: "inc1", Contact: "guard"})
	c := concluded(out)
	if len(c) != 1 || c[0].Outcome != OutcomeAcknowledged || c[0].By != "guard" {
		t.Fatalf("%+v", out)
	}
	if got := sends(tick(e, t0.Add(30*time.Minute))); len(got) != 0 {
		t.Fatalf("messages after the acknowledgement: %v", got)
	}
	if !e.NextWake().IsZero() {
		t.Fatal("no timer must survive the acknowledgement")
	}
}

func TestAnAcknowledgementFromNobodyKnownIsIgnored(t *testing.T) {
	e := newEsc(t)
	raise(e, t0, KindIntrusion)
	if out := e.Handle(Ack{Now: t0, Incident: "inc1", Contact: "stranger"}); len(out) != 0 || len(e.s.Incidents) != 1 {
		t.Fatalf("%+v", out)
	}
	if out := e.Handle(Ack{Now: t0, Incident: "other incident", Contact: "owner"}); len(out) != 0 || len(e.s.Incidents) != 1 {
		t.Fatalf("%+v", out)
	}
}

func TestClosingTheIncidentStopsTheMessages(t *testing.T) {
	e := newEsc(t)
	raise(e, t0, KindIntrusion)
	out := e.Handle(Close{Now: t0.Add(time.Minute), Incident: "inc1"})
	if c := concluded(out); len(c) != 1 || c[0].Outcome != OutcomeClosed {
		t.Fatalf("%+v", out)
	}
	if got := sends(tick(e, t0.Add(time.Hour))); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if out := e.Handle(Close{Now: t0, Incident: "inc1"}); len(out) != 0 {
		t.Fatalf("closing twice: %+v", out)
	}
}

func TestAnAcknowledgementCoversEveryKindOfTheIncident(t *testing.T) {
	e := newEsc(t)
	raise(e, t0, KindIntrusion)
	raise(e, t0, KindTamper)
	out := e.Handle(Ack{Now: t0, Incident: "inc1", Contact: "owner"})
	if len(concluded(out)) != 2 || len(e.s.Incidents) != 0 {
		t.Fatalf("%+v", out)
	}
}

// --- grouping ----------------------------------------------------------------------------

func TestDetectorsOfTheSameIncidentAreGrouped(t *testing.T) {
	e := newEsc(t)
	raise(e, t0, KindIntrusion, "dev_back")
	again := raise(e, t0.Add(5*time.Second), KindIntrusion, "dev_window", "dev_back")
	if got := sends(again); len(got) != 0 {
		t.Fatalf("the second detector must not send its own messages: %v", got)
	}
	if len(e.s.Incidents) != 1 {
		t.Fatal("one incident")
	}
	// The next message names every device concerned.
	var devices []string
	for _, a := range tick(e, t0.Add(2*time.Minute)) {
		if s, ok := a.(Send); ok {
			devices = s.Devices
		}
	}
	if !slices.Equal(devices, []string{"dev_back", "dev_window"}) {
		t.Fatalf("%v", devices)
	}
}

func TestDifferentKindsAreSeparateIncidents(t *testing.T) {
	e := newEsc(t)
	raise(e, t0, KindIntrusion)
	if got := sends(raise(e, t0, KindTamper)); len(got) == 0 || len(e.s.Incidents) != 2 {
		t.Fatalf("%v", got)
	}
}

func TestAnUnknownKindIsIgnored(t *testing.T) {
	e := newEsc(t)
	if out := raise(e, t0, "meteor"); len(out) != 0 || len(e.s.Incidents) != 0 {
		t.Fatalf("%+v", out)
	}
}

// --- fire and panic ----------------------------------------------------------------------

func TestFireTellsEveryoneAndSendsTheSMSWithTheInternet(t *testing.T) {
	e := newEsc(t)
	got := sends(raise(e, t0, KindFire, "dev_smoke"))
	want(t, "fire", got,
		"owner/app/1", "owner/sms/1", "deputy/app/1", "deputy/sms/1", "guard/sms/1", "guard/call/1",
		"manager/whatsapp/1", "manager/sms/1", "other/sms/1")
	// 60 s later, the calls are added for those who accept them.
	want(t, "insisting", sends(tick(e, t0.Add(time.Minute))), "owner/call/1")
}

// --- siren failed ------------------------------------------------------------------------

// Outside an alarm (a siren test or an arming beep, one day), a silent siren is an
// alert of its own and a loud one: it names no alarm and nothing makes it quiet.
func TestASilentSirenTellsTheFirstContactAndTheGuardOnce(t *testing.T) {
	e := newEsc(t)
	first := e.Handle(Raise{Now: t0, Incident: "inc1", Kind: KindSirenFailed, Urgency: UrgencyHigh, Devices: []string{"dev_siren1"}})
	want(t, "wave 1", sends(first), "deputy/app/1", "guard/sms/1", "guard/call/1")
	again := tick(e, t0.Add(10*time.Second))
	want(t, "internet not confirmed", sends(again), "deputy/sms/1")
	for _, a := range append(first, again...) {
		if s, ok := a.(Send); ok && (s.Urgency != UrgencyHigh || s.Alarm != "" || s.AlarmHandled) {
			t.Fatalf("%+v", s)
		}
	}
	// No reminder of its own: the alarm's plan already insists.
	end := tick(e, t0.Add(30*time.Minute))
	if c := concluded(end); len(sends(end)) != 0 || len(c) != 1 || c[0].Outcome != OutcomeUnanswered || c[0].Kind != KindSirenFailed {
		t.Fatalf("%+v", end)
	}
}

func TestFireAndPanicAreNeverThrottled(t *testing.T) {
	for _, kind := range []AlertKind{KindFire, KindPanic} {
		e := newEsc(t, func(c *Config) { c.MaxPerHour = 1 })
		got := sends(raise(e, t0, kind))
		if !slices.Contains(got, "owner/app/1") || !slices.Contains(got, "owner/sms/1") {
			t.Fatalf("%s: %v", kind, got)
		}
	}
}

func TestOtherAlertsAreThrottledPerContact(t *testing.T) {
	e := newEsc(t, func(c *Config) { c.MaxPerHour = 2 })
	out := raise(e, t0, KindIntrusion)
	// The guard gets its SMS and call (2), the deputy its app message (1).
	throttled := 0
	for _, a := range tick(e, t0.Add(2*time.Minute)) {
		if _, ok := a.(Throttled); ok {
			throttled++
		}
	}
	if throttled == 0 {
		t.Fatalf("the guard already got 2 messages this hour: wave 2 must be throttled\n%v", sends(out))
	}
	// An hour later the counter is clear.
	e.Handle(Close{Now: t0.Add(3 * time.Minute), Incident: "inc1"})
	if got := sends(e.Handle(Raise{Now: t0.Add(2 * time.Hour), Incident: "inc2", Kind: KindIntrusion, Urgency: UrgencyCritical})); len(got) != 3 {
		t.Fatalf("%v", got)
	}
}
