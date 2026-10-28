package engine

import (
	"testing"
	"time"
)

// --- the end of the entry delay waits for a code being checked ---------------

// entering arms, opens the entry door and returns when the entry delay ends.
func entering(h *harness) time.Time {
	h.t.Helper()
	h.armed()
	h.open(door)
	h.mustBe(ModeEntryDelay)
	return h.now.Add(h.cfg.EntryDelay)
}

func (h *harness) codeTyped(ref string) []Action {
	return h.e.Handle(CodeEntered{Now: h.now, Ref: ref})
}

// The right code typed in the last second of the entry delay reaches the engine after
// its end, once the hub has checked it: the alarm must not go off meanwhile.
func TestARightCodeTypedInTimeDisarmsEvenIfItsCheckEndsLate(t *testing.T) {
	h := newHarness(t)
	end := entering(h)
	h.now = end.Add(-500 * time.Millisecond)
	noActions(t, h.codeTyped("cmd-late"))
	h.mustWake(end.Add(CodeCheckHold))
	noActions(t, h.advance(1500*time.Millisecond)) // the check takes its time
	h.moves()                                      // the person walks in meanwhile
	h.mustBe(ModeEntryDelay)
	as := h.e.Handle(Command{Now: h.now, ID: "cmd-late", Name: CmdDisarm, Origin: OriginKeypad, Auth: Auth{Checked: true, OK: true, User: "u_1"}})
	h.mustBe(ModeDisarmed)
	if count[SirenOn](as) != 0 || len(alerts(as)) != 0 {
		t.Fatalf("no siren, no alert: %v", as)
	}
}

// A wrong code holds nothing once checked: the alarm goes off at once.
func TestAWrongCodeTypedInTimeLetsTheAlarmGoOffOnceChecked(t *testing.T) {
	h := newHarness(t)
	end := entering(h)
	h.now = end.Add(-time.Second)
	h.codeTyped("cmd-wrong")
	h.advance(2 * time.Second)
	h.mustBe(ModeEntryDelay)
	h.e.Handle(Command{Now: h.now, ID: "cmd-wrong", Name: CmdDisarm, Origin: OriginKeypad, Auth: Auth{Checked: true}})
	h.mustWake(end) // already passed: the next tick sets it off
	if len(eventsOf[Triggered](h.advance(0))) != 1 {
		t.Fatal("the alarm must go off")
	}
}

// The hold is bounded: a check that never ends, or a stream of codes, cannot keep the
// alarm off longer than CodeCheckHold. A code typed after the end holds nothing.
func TestTheCodeCheckHoldIsBounded(t *testing.T) {
	h := newHarness(t)
	end := entering(h)
	h.now = end.Add(-time.Second)
	h.codeTyped("cmd-1")
	h.now = end
	h.codeTyped("cmd-2")
	h.advance(CodeCheckHold - time.Millisecond)
	h.mustBe(ModeEntryDelay)
	h.advance(time.Millisecond)
	h.mustBe(ModeTriggered)

	h = newHarness(t)
	end = entering(h)
	h.now = end.Add(time.Second)
	h.codeTyped("cmd-after")
	h.mustBe(ModeTriggered)

	// Nor does a restart keep one.
	h = newHarness(t)
	end = entering(h)
	h.now = end.Add(-time.Second)
	h.codeTyped("cmd-lost")
	h.e = restart(t, h)
	h.now = end.Add(time.Second)
	h.e.Boot(h.now, Booted{}, nil)
	h.mustBe(ModeTriggered)
}
