package engine

import (
	"testing"
	"time"
)

// t0 is Saturday 2026-10-17 21:00 UTC. In these tests the shop closes at 22:00
// every day, so the notice is at 21:50 and the arming at 22:00.

func allWeek(hour, minute int) map[time.Weekday]TimeOfDay {
	m := map[time.Weekday]TimeOfDay{}
	for d := time.Sunday; d <= time.Saturday; d++ {
		m[d] = TimeOfDay{hour, minute}
	}
	return m
}

func closes22(c *Config) { c.Closing = allWeek(22, 0) }

var (
	notice  = t0.Add(50 * time.Minute) // 21:50
	closing = t0.Add(time.Hour)        // 22:00
	nextDay = closing.Add(24 * time.Hour)
)

func (h *harness) postpone() []Action { return h.cmd(CmdPostpone) }

func (h *harness) mustWake(want time.Time) {
	h.t.Helper()
	if got := h.e.NextWake(); !got.Equal(want) {
		h.t.Fatalf("NextWake %v, want %v", got, want)
	}
}

func (h *harness) mustPlan(want time.Time) {
	h.t.Helper()
	if got := h.e.State(h.now).NextScheduled; !got.Equal(want) {
		h.t.Fatalf("NextScheduled %v, want %v", got, want)
	}
}

func TestNothingIsScheduledWithoutClosingTimes(t *testing.T) {
	h := newHarness(t)
	h.mustWake(time.Time{})
	h.mustPlan(time.Time{})
	noActions(t, h.advance(48*time.Hour))
	h.mustBe(ModeDisarmed)
	if r := replyOf(t, h.postpone()); r.Status != StatusRefused || r.Reason != ReasonWrongState {
		t.Fatalf("%+v", r)
	}
}

func TestNextClosing(t *testing.T) {
	lome := time.UTC
	paris := time.FixedZone("UTC+2", 2*3600)
	weekdays := map[time.Weekday]TimeOfDay{time.Monday: {18, 30}, time.Friday: {20, 0}}
	cases := []struct {
		name string
		cfg  Config
		from time.Time
		want time.Time
		ok   bool
	}{
		{"later today", Config{Closing: allWeek(22, 0)}, t0, t0.Add(time.Hour), true},
		{"exactly at closing: tomorrow's", Config{Closing: allWeek(22, 0)}, t0.Add(time.Hour), nextDay, true},
		{"after closing: tomorrow's", Config{Closing: allWeek(22, 0)}, t0.Add(90 * time.Minute), nextDay, true},
		{"skips days without closing", Config{Closing: weekdays}, t0, time.Date(2026, 10, 19, 18, 30, 0, 0, lome), true},
		{"friday comes before monday", Config{Closing: weekdays}, time.Date(2026, 10, 19, 19, 0, 0, 0, lome), time.Date(2026, 10, 23, 20, 0, 0, 0, lome), true},
		{"a single weekday, a week later", Config{Closing: map[time.Weekday]TimeOfDay{time.Saturday: {20, 0}}}, t0, time.Date(2026, 10, 24, 20, 0, 0, 0, lome), true},
		{"site time zone", Config{Closing: allWeek(22, 0), Location: paris},
			time.Date(2026, 10, 17, 18, 0, 0, 0, lome), time.Date(2026, 10, 17, 20, 0, 0, 0, lome), true},
		{"site time zone: already past locally", Config{Closing: allWeek(22, 0), Location: paris},
			t0, time.Date(2026, 10, 18, 20, 0, 0, 0, lome), true},
		{"site time zone changes the day", Config{Closing: map[time.Weekday]TimeOfDay{time.Sunday: {1, 0}}, Location: paris},
			time.Date(2026, 10, 17, 22, 30, 0, 0, lome), time.Date(2026, 10, 17, 23, 0, 0, 0, lome), true},
		{"nothing configured", Config{}, t0, time.Time{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := c.cfg.nextClosing(c.from)
			if ok != c.ok || !got.Equal(c.want) {
				t.Fatalf("got %v %v, want %v %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestSchedulePlannedAtStart(t *testing.T) {
	h := newHarness(t, closes22)
	h.mustPlan(closing)
	h.mustWake(notice)
}

func TestNoticeThenAutomaticArming(t *testing.T) {
	h := newHarness(t, closes22)

	noActions(t, h.advance(49*time.Minute)) // 21:49
	as := h.advance(time.Minute)            // 21:50
	n := onlyEvent[ArmingScheduled](t, as)
	if !n.ScheduledFor.Equal(closing) || len(as) != 1 {
		t.Fatalf("notice: %+v in %v", n, as)
	}
	h.mustBe(ModeDisarmed)
	h.mustWake(closing)
	h.mustPlan(closing)

	noActions(t, h.advance(9*time.Minute)) // 21:59
	as = h.advance(time.Minute)            // 22:00
	h.mustBe(ModeArming)
	ev := onlyEvent[ArmingStarted](t, as)
	if ev.Origin != OriginSchedule || ev.User != "" || ev.Forced || len(ev.Bypassed) != 0 ||
		!ev.EndsAt.Equal(closing.Add(2*time.Minute)) {
		t.Fatalf("arming: %+v", ev)
	}
	if len(alerts(as)) != 0 {
		t.Fatalf("nothing to report when every zone is closed: %v", as)
	}
	h.mustPlan(time.Time{}) // the plan is consumed while arming

	as = h.advance(2 * time.Minute) // 22:02
	h.mustBe(ModeArmed)
	if a := onlyEvent[Armed](t, as); a.Origin != OriginSchedule || a.User != "" {
		t.Fatalf("armed: %+v", a)
	}
	h.mustWake(time.Time{})
	noActions(t, h.advance(24*time.Hour)) // armed: no new schedule, no new notice
}

func TestAutomaticArmingBypassesOpenZonesAndAlerts(t *testing.T) {
	h := newHarness(t, closes22)
	h.open(back)
	h.advance(50 * time.Minute)
	as := h.advance(10 * time.Minute)
	h.mustBe(ModeArming)

	ev := onlyEvent[ArmingStarted](t, as)
	if !ev.Forced || len(ev.Bypassed) != 1 || ev.Bypassed[0] != back {
		t.Fatalf("arming: %+v", ev)
	}
	al := alerts(as)
	if len(al) != 1 || al[0].Kind != AlertArmingBypassed || al[0].Urgency != UrgencyHigh ||
		len(al[0].Devices) != 1 || al[0].Devices[0] != back || al[0].Incident == "" {
		t.Fatalf("alert: %+v", al)
	}
	if count[SirenOn](as) != 0 {
		t.Fatal("no siren for a bypassed zone")
	}

	h.advance(2 * time.Minute)
	h.mustBe(ModeArmed)
	noActions(t, h.signal(back, SensorContact, true)) // stays open, stays bypassed
	h.mustBe(ModeArmed)
}

func TestPostponeBeforeTheNotice(t *testing.T) {
	h := newHarness(t, closes22)
	as := h.postpone()
	if r := replyOf(t, as); r.Status != StatusDone {
		t.Fatalf("%+v", r)
	}
	p := onlyEvent[Postponed](t, as)
	if !p.ScheduledFor.Equal(closing.Add(30*time.Minute)) || p.Count != 1 || p.Max != 3 {
		t.Fatalf("%+v", p)
	}
	h.mustBe(ModeDisarmed)
	h.mustPlan(closing.Add(30 * time.Minute))
	h.mustWake(notice.Add(30 * time.Minute)) // a new notice is due 10 minutes before

	noActions(t, h.advance(time.Hour+19*time.Minute)) // 22:19: neither the old nor the new moment has come
	as = h.advance(time.Minute)                       // 22:20
	if n := onlyEvent[ArmingScheduled](t, as); !n.ScheduledFor.Equal(closing.Add(30 * time.Minute)) {
		t.Fatalf("%+v", n)
	}
}

func TestPostponeAfterTheNoticeAnnouncesAgain(t *testing.T) {
	h := newHarness(t, closes22)
	h.advance(55 * time.Minute) // 21:55, notice already sent
	h.mustWake(closing)

	as := h.postpone()
	onlyEvent[Postponed](t, as)
	h.mustPlan(closing.Add(30 * time.Minute))
	h.mustWake(closing.Add(20 * time.Minute)) // 22:20

	noActions(t, h.advance(5*time.Minute)) // 22:00: nothing arms, it was pushed back
	h.mustBe(ModeDisarmed)

	as = h.advance(20 * time.Minute) // 22:20
	n := onlyEvent[ArmingScheduled](t, as)
	if !n.ScheduledFor.Equal(closing.Add(30 * time.Minute)) {
		t.Fatalf("%+v", n)
	}
	h.advance(10 * time.Minute) // 22:30
	h.mustBe(ModeArming)
}

func TestPostponeStopsAtTheMaximumThenTheSystemArms(t *testing.T) {
	h := newHarness(t, closes22)
	for i := 1; i <= 3; i++ {
		as := h.postpone()
		p := onlyEvent[Postponed](t, as)
		if p.Count != i || !p.ScheduledFor.Equal(closing.Add(time.Duration(i)*30*time.Minute)) {
			t.Fatalf("postponement %d: %+v", i, p)
		}
	}
	as := h.postpone()
	if r := replyOf(t, as); r.Status != StatusRefused || r.Reason != ReasonPostponeLimit {
		t.Fatalf("%+v", r)
	}
	if len(eventsOf[Postponed](as)) != 0 {
		t.Fatal("a refused postponement must not be announced")
	}
	h.mustPlan(closing.Add(90 * time.Minute))

	h.advance(2*time.Hour + 30*time.Minute) // 23:30
	h.mustBe(ModeArming)
}

func TestPostponeNeverGoesPastTheLimit(t *testing.T) {
	h := newHarness(t, closes22, func(c *Config) { c.PostponeMax = 10; c.AutoArmLimit = 45 * time.Minute })
	onlyEvent[Postponed](t, h.postpone()) // 22:30
	as := h.postpone()                    // 22:45, not 23:00
	p := onlyEvent[Postponed](t, as)
	if !p.ScheduledFor.Equal(closing.Add(45*time.Minute)) || p.Count != 2 {
		t.Fatalf("%+v", p)
	}
	if r := replyOf(t, h.postpone()); r.Reason != ReasonPostponeLimit {
		t.Fatalf("%+v", r)
	}
	h.mustPlan(closing.Add(45 * time.Minute))
}

func TestPostponementsAreCountedPerClosing(t *testing.T) {
	h := newHarness(t, closes22)
	h.postpone()
	h.postpone()
	h.advance(2 * time.Hour)   // 23:00: arming starts
	h.advance(2 * time.Minute) // 23:02
	h.mustBe(ModeArmed)
	h.cmd(CmdDisarm, goodCode("u_42")) // disarmed right after, for simplicity
	h.mustPlan(nextDay)
	for i := 1; i <= 3; i++ {
		if r := replyOf(t, h.postpone()); r.Status != StatusDone {
			t.Fatalf("postponement %d of the next closing: %+v", i, r)
		}
	}
}

func TestPostponeIsNotAllowedWhenPostponingIsForbidden(t *testing.T) {
	h := newHarness(t, closes22, func(c *Config) { c.PostponeMax = 0 })
	if r := replyOf(t, h.postpone()); r.Reason != ReasonPostponeLimit {
		t.Fatalf("%+v", r)
	}
}

func TestPostponeDuringTheExitDelayCancelsIt(t *testing.T) {
	h := newHarness(t, closes22)
	h.open(back)
	h.advance(time.Hour) // arming started at 22:00, back door bypassed
	h.mustBe(ModeArming)
	h.advance(time.Minute) // 22:01, somebody is still inside

	as := h.cmd(CmdPostpone, func(c *Command) { c.Origin = OriginApp; c.User = "u_42" })
	h.mustBe(ModeDisarmed)
	if r := replyOf(t, as); r.Status != StatusDone {
		t.Fatalf("%+v", r)
	}
	if d := onlyEvent[Disarmed](t, as); d.Origin != OriginApp || d.User != "u_42" {
		t.Fatalf("%+v", d)
	}
	if p := onlyEvent[Postponed](t, as); !p.ScheduledFor.Equal(closing.Add(30*time.Minute)) || p.Count != 1 {
		t.Fatalf("%+v", p)
	}
	if h.e.Snapshot().Bypassed[back] {
		t.Fatal("the bypass belongs to the cancelled arming")
	}
	h.mustPlan(closing.Add(30 * time.Minute))

	h.advance(19 * time.Minute) // 22:20
	h.advance(10 * time.Minute) // 22:30
	h.mustBe(ModeArming)
}

func TestPostponeDuringTheExitDelayStillRespectsTheMaximum(t *testing.T) {
	h := newHarness(t, closes22, func(c *Config) { c.PostponeMax = 1 })
	h.postpone()
	h.advance(time.Hour + 30*time.Minute) // 22:30: arming starts
	h.mustBe(ModeArming)
	if r := replyOf(t, h.postpone()); r.Reason != ReasonPostponeLimit {
		t.Fatalf("%+v", r)
	}
	h.mustBe(ModeArming)
}

func TestPostponeIsRefusedInTheWrongState(t *testing.T) {
	h := newHarness(t, closes22)
	h.armed() // manual arming: the plan is gone
	if r := replyOf(t, h.postpone()); r.Status != StatusRefused || r.Reason != ReasonWrongState {
		t.Fatalf("armed: %+v", r)
	}

	h = newHarness(t, closes22)
	h.arm() // manual exit delay: nothing to postpone
	if r := replyOf(t, h.postpone()); r.Reason != ReasonWrongState {
		t.Fatalf("manual arming: %+v", r)
	}
	h.mustBe(ModeArming)

	h = newHarness(t, closes22)
	h.armed()
	h.breakWindow()
	if r := replyOf(t, h.postpone()); r.Reason != ReasonWrongState {
		t.Fatalf("triggered: %+v", r)
	}
}

func TestPostponeNeedsNoCode(t *testing.T) {
	h := newHarness(t, closes22)
	as := h.postpone() // no Auth at all
	if r := replyOf(t, as); r.Status != StatusDone {
		t.Fatalf("%+v", r)
	}
	if len(h.e.Snapshot().Failures) != 0 {
		t.Fatal("postponing must not count as a wrong code")
	}
}

func TestDisarmDuringAutomaticExitDelayWaitsForTomorrow(t *testing.T) {
	h := newHarness(t, closes22)
	h.advance(time.Hour) // 22:00
	h.advance(time.Minute)
	h.mustBe(ModeArming)
	h.cmd(CmdDisarm, goodCode("u_42"))
	h.mustBe(ModeDisarmed)
	h.mustPlan(nextDay)
	noActions(t, h.advance(10*time.Minute)) // not armed again tonight
	h.mustBe(ModeDisarmed)
}

func TestManualArmingDropsTheSchedule(t *testing.T) {
	h := newHarness(t, closes22)
	h.advance(10 * time.Minute) // 21:10
	h.armed()
	h.mustPlan(time.Time{})
	h.mustWake(time.Time{})
	noActions(t, h.advance(time.Hour)) // no notice, no second arming at 22:00
	h.mustBe(ModeArmed)
}

func TestDisarmingPlansTheNextClosing(t *testing.T) {
	h := newHarness(t, closes22)
	h.advance(10 * time.Minute)
	h.armed()
	h.advance(10 * time.Minute) // 21:22
	h.cmd(CmdDisarm, goodCode("u_42"))
	h.mustPlan(closing) // still before closing time: it arms again tonight
	h.mustWake(notice)

	h.advance(38 * time.Minute) // 22:00, the arming starts
	h.advance(h.cfg.ExitDelay)  // and has run
	h.mustBe(ModeArmed)
	h.cmd(CmdDisarm, goodCode("u_42"))
	h.mustPlan(nextDay)
}

func TestDisarmingJustBeforeClosingGivesALateNoticeAtOnce(t *testing.T) {
	h := newHarness(t, closes22)
	h.armed()
	h.advance(53 * time.Minute) // 21:55
	h.cmd(CmdDisarm, goodCode("u_42"))
	h.mustWake(h.now) // the notice is due now, not in the past
	n := onlyEvent[ArmingScheduled](t, h.advance(0))
	if !n.ScheduledFor.Equal(closing) {
		t.Fatalf("%+v", n)
	}
}

// A tick a little late replays the notice, the arming and the end of the exit delay
// at their own time.
func TestLateTickReplaysNoticeThenArmingThenArmed(t *testing.T) {
	h := newHarness(t, closes22)
	as := h.advance(time.Hour + 30*time.Second) // 22:00:30
	h.mustBe(ModeArming)
	onlyEvent[ArmingScheduled](t, as)
	if ev := onlyEvent[ArmingStarted](t, as); !ev.EndsAt.Equal(closing.Add(h.cfg.ExitDelay)) {
		t.Fatalf("a tick 30 s late replays the arming at its time: %+v", ev)
	}
	onlyEvent[Armed](t, h.advance(time.Minute+40*time.Second)) // 22:02:10
}

// The hub was off (or suspended) at closing time and comes back before the plan's
// limit: the arming still happens, but its exit delay starts now, so that whoever is
// inside gets the whole delay to postpone it or to leave. Replayed at its own time,
// it would arm at once and the first movement inside would ring.
func TestAnArmingMissedWithinTheLimitStartsItsExitDelayNow(t *testing.T) {
	h := newHarness(t, closes22)
	as := h.advance(2*time.Hour + 30*time.Minute) // off until 23:30, before the limit (00:00)
	h.mustBe(ModeArming)
	onlyEvent[ArmingScheduled](t, as)
	if ev := onlyEvent[ArmingStarted](t, as); !ev.EndsAt.Equal(h.now.Add(h.cfg.ExitDelay)) || ev.Origin != OriginSchedule {
		t.Fatalf("the exit delay must start now: %+v", ev)
	}
	if r := replyOf(t, h.postpone()); r.Status != StatusDone {
		t.Fatalf("the people inside can still postpone it: %+v", r)
	}
}

// Back after the plan's limit, the next morning: yesterday's arming is not replayed on
// a shop that opened again; the next closing is planned.
func TestAnArmingMissedPastTheLimitIsDropped(t *testing.T) {
	h := newHarness(t, closes22)
	h.e = restart(t, h)
	h.now = t0.Add(11 * time.Hour) // 08:00 the next day
	as := h.e.Boot(h.now, []Signal{
		{DeviceID: motion, Sensor: SensorMotion, Active: true},
		{DeviceID: door, Sensor: SensorContact, Active: true},
	})
	h.mustBe(ModeDisarmed)
	if len(eventsOf[ArmingScheduled](as)) != 0 || len(eventsOf[ArmingStarted](as)) != 0 || len(alerts(as)) != 0 {
		t.Fatalf("nothing of last night's arming: %v", as)
	}
	h.mustPlan(nextDay)
	h.mustWake(nextDay.Add(-h.cfg.AutoArmNotice))

	// The same within a run: a tick that comes the next morning.
	h = newHarness(t, closes22)
	noActions(t, h.advance(11*time.Hour))
	h.mustBe(ModeDisarmed)
	h.mustPlan(nextDay)
}

func TestAlarmWhileDisarmedPausesTheScheduleThenPlansAgain(t *testing.T) {
	h := newHarness(t, closes22)
	h.advance(10 * time.Minute)
	h.signal(smoke, SensorSmoke, true)
	h.mustBe(ModeTriggered)
	h.mustPlan(time.Time{})
	h.cmd(CmdSilence, goodCode("u_42"))
	h.mustBe(ModeDisarmed)
	h.mustPlan(closing)
	h.mustWake(notice)
}

func TestScheduleSurvivesARestart(t *testing.T) {
	h := newHarness(t, closes22)
	h.postpone()
	h.advance(time.Hour + 25*time.Minute) // 22:25: second notice sent
	want := h.e.Snapshot().Schedule
	if want == nil || want.Postponed != 1 || !want.Announced {
		t.Fatalf("%+v", want)
	}

	h.e = restart(t, h)
	h.e.Boot(h.now, nil)
	got := h.e.Snapshot().Schedule
	if got == nil || *got != *want {
		t.Fatalf("after restart %+v, want %+v", got, want)
	}
	h.mustWake(closing.Add(30 * time.Minute))
	h.advance(5 * time.Minute)
	h.mustBe(ModeArming)
}

func TestRestartDuringAutomaticExitDelayKeepsItPostponable(t *testing.T) {
	h := newHarness(t, closes22)
	h.advance(time.Hour + time.Minute)
	h.mustBe(ModeArming)
	h.e = restart(t, h)
	h.e.Boot(h.now, nil)
	h.mustBe(ModeArming)
	if r := replyOf(t, h.postpone()); r.Status != StatusDone {
		t.Fatalf("%+v", r)
	}
	h.mustBe(ModeDisarmed)
}

func TestBootPlansWhenTheSnapshotHasNoSchedule(t *testing.T) {
	h := newHarness(t) // saved without a schedule
	h.cfg.Closing = allWeek(22, 0)
	h.e = restart(t, h) // restarted with a schedule configured
	h.mustPlan(time.Time{})
	h.e.Boot(h.now, nil)
	h.mustPlan(closing)
	h.mustWake(notice)
}

func TestScheduleIsCopiedInSnapshots(t *testing.T) {
	h := newHarness(t, closes22)
	snap := h.e.Snapshot()
	snap.Schedule.Postponed = 99
	if h.e.Snapshot().Schedule.Postponed != 0 {
		t.Fatal("changing a copy must not change the engine")
	}
}

// An automatic arming planned while the clock ran ahead is planned again once the
// time goes back: the closing it skipped as past may still be to come today.
func TestTheScheduleIsPlannedAgainWhenTheTimeGoesBack(t *testing.T) {
	h := newHarness(t, closes22)
	h.advance(70 * time.Minute) // 22:10 on a clock an hour ahead: the shop armed itself
	h.cmd(CmdDisarm, goodCode("u_owner"))
	h.mustPlan(nextDay)
	h.now = t0.Add(10 * time.Minute) // 21:10: the clock is corrected
	h.e.Handle(Tick{Now: h.now})
	h.mustPlan(closing)
}
