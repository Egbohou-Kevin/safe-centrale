package engine

import (
	"testing"
	"time"
)

// The system clock stepped forward (a Pi without a battery clock set by NTP, see
// Stepped): the siren cycle under way and a lockout keep the time that really passed.
// The siren is not cut short, the lockout is not lifted, and the alarm's duration
// counts only what really passed.
func TestAClockSetForwardCutsNoSirenAndLiftsNoLockout(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	for range 3 {
		h.cmd(CmdDisarm, badCode)
	}
	h.advance(time.Minute)
	const step = 14 * time.Hour
	h.e.Stepped(step)
	h.now = h.now.Add(step)
	if as := h.advance(time.Minute); len(as) != 0 {
		t.Fatalf("two minutes into the alarm, after a clock set forward: %v", as)
	}
	if s := h.e.State(h.now); s.Mode != ModeTriggered || !s.SirenOn || s.Cycle != 1 || s.LockedUntil.IsZero() {
		t.Fatalf("after a clock set forward: %+v", s)
	}
	if r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner"))); r.Reason != ReasonLockedOut {
		t.Fatalf("the lockout was lifted: %+v", r)
	}
	h.advance(8 * time.Minute)
	as := h.cmd(CmdDisarm, goodCode("u_owner"))
	if c := onlyEvent[Cleared](t, as); c.Duration != 10*time.Minute {
		t.Fatalf("cleared after %v, want 10m", c.Duration)
	}
}
