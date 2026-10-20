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
	if got := h.e.Snapshot().Failed; got != 0 {
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
