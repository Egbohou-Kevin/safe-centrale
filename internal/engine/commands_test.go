package engine

import (
	"testing"
	"time"
)

// --- codes, lockout ---------------------------------------------------------

func TestBadCodesLockOutThenRecover(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()

	// 1st and 2nd wrong codes: refusal and event, no lockout yet.
	for i := 1; i <= 2; i++ {
		as := h.cmd(CmdDisarm, badCode)
		r := replyOf(t, as)
		if r.Status != StatusRefused || r.Reason != ReasonBadCode {
			t.Fatalf("attempt %d: %+v", i, r)
		}
		af := onlyEvent[AuthFailed](t, as)
		if af.Attempts != i || !af.LockedUntil.IsZero() || len(alerts(as)) != 0 {
			t.Fatalf("attempt %d: %+v alerts=%v", i, af, alerts(as))
		}
		h.mustBe(ModeTriggered)
	}

	// 3rd wrong code: locked for 10 minutes, with an alert.
	as := h.cmd(CmdDisarm, badCode)
	af := onlyEvent[AuthFailed](t, as)
	if af.Attempts != 3 || !af.LockedUntil.Equal(h.now.Add(10*time.Minute)) {
		t.Fatalf("lockout: %+v", af)
	}
	if al := alerts(as); len(al) != 1 || al[0].Kind != AlertAuthFailed {
		t.Fatalf("lockout alert: %v", al)
	}
	if got := h.e.State(h.now).LockedUntil; got.IsZero() {
		t.Fatal("State must announce the lockout")
	}

	// During the lockout, even the right code is refused.
	h.advance(9 * time.Minute)
	r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner")))
	if r.Status != StatusRefused || r.Reason != ReasonLockedOut {
		t.Fatalf("during the lockout: %+v", r)
	}
	h.mustBe(ModeTriggered)
	// And a wrong code during the lockout does not count.
	if got := len(h.e.Snapshot().Failures); got != 0 {
		t.Fatalf("counter %d", got)
	}
	r = replyOf(t, h.cmd(CmdDisarm, badCode))
	if r.Reason != ReasonLockedOut {
		t.Fatalf("wrong code during the lockout: %+v", r)
	}

	// After the 10 minutes, the right code works.
	h.advance(time.Minute)
	if got := h.e.State(h.now).LockedUntil; !got.IsZero() {
		t.Fatal("the lockout is over")
	}
	r = replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner")))
	if r.Status != StatusDone {
		t.Fatalf("after the lockout: %+v", r)
	}
	h.mustBe(ModeDisarmed)
}

func TestGoodCodeResetsFailureCounter(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.cmd(CmdDisarm, badCode)
	h.cmd(CmdDisarm, badCode)
	h.cmd(CmdDisarm, goodCode("u_42"))
	h.mustBe(ModeDisarmed)
	h.armed()
	as := h.cmd(CmdDisarm, badCode)
	if af := onlyEvent[AuthFailed](t, as); af.Attempts != 1 {
		t.Fatalf("the counter must start over: %+v", af)
	}
}

// badAt enters a wrong code after d and returns how it was counted.
func (h *harness) badAt(d time.Duration, origin Origin) AuthFailed {
	h.t.Helper()
	h.advance(d)
	return onlyEvent[AuthFailed](h.t, h.cmd(CmdDisarm, badCode, func(c *Command) { c.Origin = origin }))
}

// Wrong codes spread over hours never lock: only those of the last LockoutWindow
// count, the window sliding with each new one.
func TestOnlyTheWrongCodesOfTheLastTenMinutesCount(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.badAt(0, OriginApp)
	h.badAt(time.Minute, OriginApp)
	// 11 minutes after the first two: they are forgotten.
	if af := h.badAt(11*time.Minute, OriginApp); af.Attempts != 1 || !af.LockedUntil.IsZero() {
		t.Fatalf("a wrong code 11 min after two others: %+v", af)
	}
	if af := h.badAt(5*time.Minute, OriginApp); af.Attempts != 2 || !af.LockedUntil.IsZero() {
		t.Fatalf("second within the window: %+v", af)
	}
	// 10 minutes after the first of the two: it just left the window.
	if af := h.badAt(5*time.Minute, OriginApp); af.Attempts != 2 || !af.LockedUntil.IsZero() {
		t.Fatalf("a wrong code exactly a window after another: %+v", af)
	}
	if got := h.e.State(h.now).LockedUntil; !got.IsZero() || LockoutWindow != 10*time.Minute {
		t.Fatalf("locked until %v, window %v", got, LockoutWindow)
	}
	h.mustBe(ModeArmed)
}

// Three wrong codes within 10 minutes lock the whole site for LockoutDuration,
// whether they came from the keypad or the app.
func TestThreeWrongCodesWithinTenMinutesLockTheSite(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.badAt(0, OriginKeypad)
	if af := h.badAt(5*time.Minute, OriginApp); af.Attempts != 2 {
		t.Fatalf("the keypad and the app count together: %+v", af)
	}
	af := h.badAt(5*time.Minute-time.Second, OriginKeypad)
	if af.Attempts != 3 || !af.LockedUntil.Equal(h.now.Add(h.cfg.LockoutDuration)) {
		t.Fatalf("three within 10 minutes: %+v", af)
	}
	if r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner"), func(c *Command) { c.Origin = OriginApp })); r.Reason != ReasonLockedOut {
		t.Fatalf("the lockout holds for every origin: %+v", r)
	}
	if n := len(h.e.Snapshot().Failures); n != 0 {
		t.Fatalf("a lockout starts the count over: %d left", n)
	}
}

// A patient guesser stays under the window: two wrong codes every 10 minutes (the
// cloud's cap for one phone) never lock. Every third wrong code since the last good
// one alerts the contacts all the same, without locking. The count survives a
// restart, and a good code starts it over.
func TestAPatientGuesserAlertsTheContactsWithoutLocking(t *testing.T) {
	h := newHarness(t)
	h.armed()
	guess := func(i int) {
		t.Helper()
		as := h.cmd(CmdDisarm, badCode, func(c *Command) { c.Origin = OriginApp })
		af, al := onlyEvent[AuthFailed](t, as), alerts(as)
		if !af.LockedUntil.IsZero() || replyOf(t, as).Reason != ReasonBadCode {
			t.Fatalf("guess %d locked: %+v", i, af)
		}
		alerted := len(al) == 1 && al[0].Kind == AlertAuthFailed
		if alerted != (i%3 == 0) || len(al) > 1 {
			t.Fatalf("guess %d: alerts %v", i, al)
		}
		h.advance(6 * time.Minute)
	}
	for i := 1; i <= 4; i++ {
		guess(i)
	}
	h.e = restart(t, h)
	h.e.Boot(h.now, nil)
	for i := 5; i <= 7; i++ {
		guess(i)
	}
	h.mustBe(ModeArmed)

	h.cmd(CmdDisarm, goodCode("u_owner"))
	h.armed()
	for i := 1; i <= 3; i++ {
		guess(i)
	}
}

// When the third wrong code since the last good one also locks, the contacts get one
// alert, not two.
func TestALockoutThatIsAlsoTheThirdWrongCodeAlertsOnce(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.badAt(0, OriginKeypad)
	h.badAt(time.Minute, OriginKeypad)
	as := h.cmd(CmdDisarm, badCode)
	if al := alerts(as); len(al) != 1 || onlyEvent[AuthFailed](t, as).LockedUntil.IsZero() {
		t.Fatalf("alerts %v in %v", al, as)
	}
}

func TestUncheckedCodeCountsAsBadCode(t *testing.T) {
	h := newHarness(t)
	h.armed()
	r := replyOf(t, h.cmd(CmdDisarm)) // the driver checked no code
	if r.Status != StatusRefused || r.Reason != ReasonBadCode {
		t.Fatalf("a disarm with no checked code must be refused: %+v", r)
	}
	h.mustBe(ModeArmed)
}

func TestArmNeedsNoCodeAndIgnoresLockout(t *testing.T) {
	h := newHarness(t)
	h.cmd(CmdDisarm, badCode)
	h.cmd(CmdDisarm, badCode)
	h.cmd(CmdDisarm, badCode) // locked
	r := replyOf(t, h.arm())
	if r.Status != StatusDone {
		t.Fatalf("arming stays allowed during the lockout: %+v", r)
	}
}

func TestDisarmWhenAlreadyDisarmedIsIdempotent(t *testing.T) {
	h := newHarness(t)
	as := h.cmd(CmdDisarm, goodCode("u_42"))
	if r := replyOf(t, as); r.Status != StatusDone || len(eventsOf[Disarmed](as)) != 0 {
		t.Fatalf("%v", as)
	}
}

// --- orders: expiry, unknown ------------------------------------------------

func TestExpiredCommandIsNotExecuted(t *testing.T) {
	h := newHarness(t)
	as := h.arm(func(c *Command) { c.Exp = h.now.Add(-time.Second) })
	h.mustBe(ModeDisarmed)
	if r := replyOf(t, as); r.Status != StatusExpired {
		t.Fatalf("reply %+v", r)
	}
	// Exactly at the expiry time, the order still goes through.
	h.arm(func(c *Command) { c.Exp = h.now })
	h.mustBe(ModeArming)
}

func TestUnknownCommand(t *testing.T) {
	h := newHarness(t)
	r := replyOf(t, h.cmd("explode"))
	if r.Status != StatusFailed || r.Reason != ReasonUnknownCommand {
		t.Errorf("%+v", r)
	}
}

func TestRepliesCarryCommandID(t *testing.T) {
	h := newHarness(t)
	as := h.e.Handle(Command{Now: h.now, ID: "my-order", Name: CmdArm, Origin: OriginApp})
	if r := replyOf(t, as); r.Ref != "my-order" {
		t.Fatalf("ref %q", r.Ref)
	}
}

func TestArmRefusedDuringEntryDelayAndAlarm(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.open(door)
	if r := replyOf(t, h.arm()); r.Reason != ReasonWrongState {
		t.Fatalf("%+v", r)
	}
	h.advance(45 * time.Second)
	if r := replyOf(t, h.arm()); r.Reason != ReasonWrongState {
		t.Fatalf("%+v", r)
	}
}
