package engine

import (
	"testing"
	"time"
)

// --- a movement from before the mode, the clock set back while the engine ran -------

// movedBeforeTheMode arms, a movement heard during the exit delay and still active, the
// site armed 30 s, the system started 20 minutes before. It returns the movement as
// the broker keeps it and when the system started.
func movedBeforeTheMode(h *harness) (Signal, time.Time) {
	booted := h.now.Add(-20 * time.Minute)
	h.arm()
	h.advance(10 * time.Second)
	sig := at(motion, SensorMotion, true, h.now)
	h.moves()
	h.advance(h.cfg.ExitDelay)
	h.mustBe(ModeArmed)
	h.advance(30 * time.Second)
	return sig, booted
}

// A movement active since before the mode does not ring at a restart (the negative
// case): the start of the mode tells it, or, the clock set back an hour while the engine
// ran, the time a Boot before set it aside does (SetAside). This is the restored engine
// of the random sequences, right where the live one rang (28 October 2026, seed 169
// shifted); the live one now sets it aside too as the clock goes back (see
// restart9_test.go).
func TestAMovementFromBeforeTheModeDoesNotRingAgain(t *testing.T) {
	for _, back := range []bool{false, true} {
		h := newHarness(t)
		sig, booted := movedBeforeTheMode(h)
		h.e = restart(t, h)
		if err := onlySiren(h.e.Boot(h.now, Booted{At: booted}, []Signal{sig})); err != nil {
			t.Fatalf("the first restart: %v", err)
		}
		if back {
			h.now, booted = h.now.Add(-time.Hour), booted.Add(-time.Hour)
			noActions(t, h.e.Handle(Tick{Now: h.now}))
		}
		h.e = restart(t, h)
		if err := onlySiren(h.e.Boot(h.now, Booted{At: booted}, []Signal{sig})); err != nil {
			t.Fatalf("the clock set back %v, the second restart: %v", back, err)
		}
		h.mustBe(ModeArmed)
	}
}

// --- the trace of the step at boot, a live step forward since ---------------------

// firstBoot plays the hub's first Boot of a system run where NTP set the clock forward
// by step before the hub started (none when zero: the trace then tells the start as it
// is, and the hub gives it all the same): it returns what Boot was given.
func firstBoot(t *testing.T, h *harness, step time.Duration) Booted {
	t.Helper()
	h.e = restart(t, h)
	booted := Booted{At: h.now.Add(-30 * time.Second)}
	booted.Shown = booted.At.Add(-step)
	if err := onlySiren(h.e.Boot(h.now, booted, nil)); err != nil {
		t.Fatal(err)
	}
	return booted
}

// NTP sets the clock forward again while the hub runs (internet back hours after the
// boot), then the hub restarts within the same system run: the trace of the start,
// read again, would take every state heard before the live step for one dated before
// NTP set the clock at boot, news. A movement heard during the exit delay rang then (28
// October 2026, the shifted random sequences, seed 455). The trace serves the first
// Boot of a run only, a step at boot or none: the same day, a first Boot without a step
// was given no trace by the hub, and the one read after the live step served
// (relecture; the hub's witness is TestARestartAfterALiveStepDoesNotRingAMovementHeardBefore).
func TestALiveStepForwardDoesNotMakeAMovementHeardNewsAgain(t *testing.T) {
	for _, step := range []time.Duration{time.Minute, 0} {
		h := newHarness(t)
		booted := firstBoot(t, h, step)
		sig, _ := movedBeforeTheMode(h)
		h.e.Stepped(14 * time.Hour)
		h.now, booted.At = h.now.Add(14*time.Hour), booted.At.Add(14*time.Hour)
		noActions(t, h.e.Handle(Tick{Now: h.now}))
		h.e = restart(t, h)
		if err := onlySiren(h.e.Boot(h.now, booted, []Signal{sig})); err != nil {
			t.Fatalf("a step of %v at boot, the movement heard before the live step: %v", step, err)
		}
		h.mustBe(ModeArmed)
	}
}

// The positive case: after a reboot, the new trace serves its run's first Boot. A door
// opened in the seconds after the power came back, dated on the clock as it read
// before NTP set it forward, before the time the engine last heard it closed, is news.
func TestANewSystemRunsTraceStillTellsAStateDatedBeforeTheStep(t *testing.T) {
	h := newHarness(t)
	firstBoot(t, h, time.Minute)
	h.armed()
	h.advance(time.Minute)
	h.close(door)
	h.advance(time.Minute)
	h.e = restart(t, h)
	back := h.now.Add(-10 * time.Minute)
	booted := steppedBoot(h, back)
	opened := at(door, SensorContact, true, back.Add(time.Second))
	if as := h.e.Boot(h.now, booted, []Signal{opened}); !acted(as) {
		t.Fatalf("a door opened before NTP set the clock forward, a new run: %v", as)
	}
}
