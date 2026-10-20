package escalation

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
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
	if e.Active() == 0 {
		t.Fatal("the last reminders still wait for their confirmations")
	}

	// Nobody answered: the incident ends as unanswered once the timers are done.
	end := tick(e, t0.Add(40*time.Minute))
	if c := concluded(end); len(c) != 1 || c[0].Outcome != OutcomeUnanswered || c[0].Kind != KindIntrusion {
		t.Fatalf("%+v", end)
	}
	if e.Active() != 0 || !e.NextWake().IsZero() {
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
	if out := e.Handle(Ack{Now: t0, Incident: "inc1", Contact: "stranger"}); len(out) != 0 || e.Active() != 1 {
		t.Fatalf("%+v", out)
	}
	if out := e.Handle(Ack{Now: t0, Incident: "other incident", Contact: "owner"}); len(out) != 0 || e.Active() != 1 {
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
	if len(concluded(out)) != 2 || e.Active() != 0 {
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
	if e.Active() != 1 {
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
	if got := sends(raise(e, t0, KindTamper)); len(got) == 0 || e.Active() != 2 {
		t.Fatalf("%v", got)
	}
}

func TestAnUnknownKindIsIgnored(t *testing.T) {
	e := newEsc(t)
	if out := raise(e, t0, "meteor"); len(out) != 0 || e.Active() != 0 {
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
	want(t, "uplink back", sends(e.Handle(Uplink{Now: t0.Add(time.Hour), Up: true})), "owner/app/1")
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
	play := func() []string {
		e := newEsc(t)
		var all []string
		all = append(all, sends(raise(e, t0, KindIntrusion, "dev_back"))...)
		for _, d := range []time.Duration{10 * time.Second, 70 * time.Second, 2 * time.Minute, 6 * time.Minute, time.Hour} {
			all = append(all, sends(tick(e, t0.Add(d)))...)
		}
		return all
	}
	if a, b := play(), play(); !slices.Equal(a, b) {
		t.Fatalf("two identical runs differ:\n%v\n%v", a, b)
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

// --- texts -------------------------------------------------------------------------------

func TestTextsAreShortPlainASCIIAndSayWhatToDo(t *testing.T) {
	e := newEsc(t, func(c *Config) { c.Location = time.FixedZone("UTC+1", 3600) })
	out := raise(e, t0, KindIntrusion, "dev_back")
	var text string
	for _, a := range out {
		if s, ok := a.(Send); ok {
			text = s.Text
		}
	}
	for _, part := range []string{"ALARME", "Boutique Lome", "Porte arriere", "22:04", "Repondre 1"} {
		if !strings.Contains(text, part) {
			t.Errorf("%q misses %q", text, part)
		}
	}
	checkText(t, text)
}

func checkText(t *testing.T, text string) {
	t.Helper()
	if len(text) > 160 {
		t.Errorf("%d characters: %q", len(text), text)
	}
	for _, r := range text {
		if r < 0x20 || r > 0x7e {
			t.Errorf("%q has a character outside plain ASCII: %q", text, r)
		}
	}
}

func TestEveryKindHasASuitableText(t *testing.T) {
	for _, kind := range allKinds {
		e := newEsc(t)
		out := e.Handle(Raise{Now: t0, Incident: "x", Kind: kind, Urgency: UrgencyHigh, Percent: 40, Devices: []string{"dev_back", "dev_a"}})
		found := false
		for _, a := range out {
			if s, ok := a.(Send); ok {
				found = true
				checkText(t, s.Text)
				if !strings.Contains(s.Text, "Boutique Lome") {
					t.Errorf("%s: %q does not name the site", kind, s.Text)
				}
				ack := strings.Contains(s.Text, "Repondre 1")
				wantAck := kind != KindMainsRestored && kind != KindDeviceOffline && kind != KindBatteryLow && kind != KindGSMDegraded
				if ack != wantAck {
					t.Errorf("%s: answer hint present=%v, want %v: %q", kind, ack, wantAck, s.Text)
				}
			}
		}
		if !found {
			t.Errorf("%s: nothing sent", kind)
		}
	}
}

func TestALongTextIsCutButKeepsTheWayToAnswer(t *testing.T) {
	e := newEsc(t, func(c *Config) { c.SiteName = strings.Repeat("Grande Boutique ", 5) })
	long := make([]string, 6)
	for i := range long {
		long[i] = fmt.Sprintf("dev_%d", i)
	}
	out := raise(e, t0, KindIntrusion, long...)
	for _, a := range out {
		if s, ok := a.(Send); ok {
			checkText(t, s.Text)
			if !strings.HasSuffix(s.Text, "Repondre 1 si vous gerez.") || !strings.Contains(s.Text, "...") {
				t.Fatalf("%q", s.Text)
			}
		}
	}
}

func TestASCIIRemovesAccentsAndOddCharacters(t *testing.T) {
	if got := ascii("Boutique Lom\u00e9 \u00c9t\u00e9 \u2019 \u4e2d\u6587 \u0153uf"); got != "Boutique Lome Ete ' oeuf" && got != "Boutique Lome Ete '  oeuf" {
		t.Fatalf("%q", got)
	}
}

// --- configuration -----------------------------------------------------------------------

func TestConfigValidation(t *testing.T) {
	if _, err := New(DefaultConfig("Shop", contacts())); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Config){
		"no site name": func(c *Config) { c.SiteName = "" },
		"six contacts": func(c *Config) {
			c.Contacts = append(c.Contacts, Contact{ID: "x", Role: RoleOther, Channels: []Channel{ChannelSMS}})
		},
		"no id":             func(c *Config) { c.Contacts[1].ID = "" },
		"duplicate id":      func(c *Config) { c.Contacts[1].ID = "owner" },
		"unknown role":      func(c *Config) { c.Contacts[1].Role = "king" },
		"no channel":        func(c *Config) { c.Contacts[1].Channels = nil },
		"unknown channel":   func(c *Config) { c.Contacts[1].Channels = []Channel{"pigeon"} },
		"channel twice":     func(c *Config) { c.Contacts[1].Channels = []Channel{ChannelSMS, ChannelSMS} },
		"no owner":          func(c *Config) { c.Contacts = c.Contacts[1:] },
		"no confirm delay":  func(c *Config) { c.InternetConfirm = 0 },
		"no gsm delay":      func(c *Config) { c.GSMDelay = 0 },
		"no hourly limit":   func(c *Config) { c.MaxPerHour = 0 },
		"unknown kind":      func(c *Config) { c.Plans["meteor"] = Plan{Waves: []Wave{{To: []Audience{AudienceOwner}}}} },
		"plan without wave": func(c *Config) { c.Plans[KindFire] = Plan{} },
		"waves out of order": func(c *Config) {
			c.Plans[KindFire] = Plan{Waves: []Wave{{After: time.Minute, To: []Audience{AudienceOwner}}, {To: []Audience{AudienceOwner}}}}
		},
		"wave tells nobody": func(c *Config) { c.Plans[KindFire] = Plan{Waves: []Wave{{}}} },
		"unknown audience":  func(c *Config) { c.Plans[KindFire] = Plan{Waves: []Wave{{To: []Audience{"all the world"}}}} },
		"reminders no delay": func(c *Config) {
			c.Plans[KindFire] = Plan{Waves: []Wave{{To: []Audience{AudienceOwner}}}, Repeat: Repeat{Times: 2}}
		},
		"negative reminders": func(c *Config) {
			c.Plans[KindFire] = Plan{Waves: []Wave{{To: []Audience{AudienceOwner}}}, Repeat: Repeat{Times: -1, Every: time.Minute}}
		},
		"gsm after on normal": func(c *Config) {
			c.Plans[KindFire] = Plan{Waves: []Wave{{To: []Audience{AudienceOwner}}}, GSMAfter: time.Hour}
		},
		"negative gsm after": func(c *Config) {
			c.Plans[KindFire] = Plan{Waves: []Wave{{To: []Audience{AudienceOwner}}}, InternetOnly: true, GSMAfter: -1}
		},
	}
	for name, mutate := range bad {
		cfg := DefaultConfig("Shop", contacts())
		cfg.Contacts = slices.Clone(cfg.Contacts)
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// No contact at all is allowed (the contacts come later).
	cfg := DefaultConfig("Shop", nil)
	if _, err := New(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestMissingPlansTakeTheDefault(t *testing.T) {
	e := newEsc(t, func(c *Config) { c.Plans = nil })
	if got := sends(raise(e, t0, KindIntrusion)); len(got) == 0 {
		t.Fatal("the default plan must apply")
	}
}

func TestDescribe(t *testing.T) {
	for _, a := range []Action{Send{Kind: KindFire, Contact: "c", Channel: ChannelSMS, Wave: 1}, Throttled{Incident: "i"}, Concluded{Incident: "i", Outcome: OutcomeClosed}} {
		if Describe(a) == "" {
			t.Fatalf("%T", a)
		}
	}
	if Describe(nil) == "" {
		t.Fatal("nil")
	}
}

func TestTheHourlyLimitIsExactlyTheConfiguredMaximum(t *testing.T) {
	e := newEsc(t, func(c *Config) {
		c.MaxPerHour = 3
		c.Contacts = []Contact{{ID: "owner", Role: RoleOwner, Channels: []Channel{ChannelApp, ChannelSMS}}}
	})
	want(t, "message 1", sends(raise(e, t0, KindIntrusion)), "owner/app/1")
	want(t, "message 2", sends(tick(e, t0.Add(10*time.Second))), "owner/sms/1")
	want(t, "message 3", sends(tick(e, t0.Add(2*time.Minute))), "owner/app/2")
	out := tick(e, t0.Add(2*time.Minute+10*time.Second))
	if len(sends(out)) != 0 {
		t.Fatalf("a fourth message in the hour must be held back: %v", sends(out))
	}
	throttled := false
	for _, a := range out {
		_, ok := a.(Throttled)
		throttled = throttled || ok
	}
	if !throttled {
		t.Fatal("the held back message must be reported")
	}
}
