package engine

import (
	"encoding/json"
	"testing"
	"time"
)

// --- a movement from before the mode, the clock set back while the engine ran -------

// setBackLive sets the clock back d while the engine runs, booted (when the system
// started) moving with it, and returns the latter.
func setBackLive(t *testing.T, h *harness, booted time.Time, d time.Duration) time.Time {
	t.Helper()
	h.now = h.now.Add(-d)
	noActions(t, h.e.Handle(Tick{Now: h.now}))
	return booted.Add(-d)
}

// The clock set back an hour while the engine ran, a movement active since before the
// mode, never set aside by a Boot, does not ring at the next restart, nor at the one
// after. It rang until 28 October 2026 (the limit written in docs/engine.md, frozen by
// TestAMovementFromBeforeTheModeNeverSetAsideRingsAfterTheClockWentBack, which this test
// replaces): the start of the mode, brought back to the time the clock went back to, no
// longer told it from a movement dated since. The engine now sets it aside (SetAside) the
// moment it brings that start back, by the time it began: given back with that very
// time, it is the same movement.
func TestAMovementFromBeforeTheModeDoesNotRingAfterTheClockWentBackLive(t *testing.T) {
	h := newHarness(t)
	sig, booted := movedBeforeTheMode(h)
	booted = setBackLive(t, h, booted, time.Hour)
	for i := range 2 {
		h.e = restart(t, h)
		if err := onlySiren(h.e.Boot(h.now, Booted{At: booted}, []Signal{sig})); err != nil {
			t.Fatalf("restart %d after the clock went back: %v", i+1, err)
		}
		h.mustBe(ModeArmed)
		h.advance(time.Minute)
	}
}

// So too a movement heard during an exit delay the clock set back interrupted: Boot
// takes it for no news while it is under way (see older). Restarted once the exit delay
// brought back has ended, or after the site armed live, it rang.
func TestAMovementDuringTheExitDelayDoesNotRingAfterTheClockWentBackLive(t *testing.T) {
	for _, live := range []bool{true, false} {
		h := newHarness(t)
		booted := h.now.Add(-20 * time.Minute)
		h.arm()
		h.advance(10 * time.Second)
		sig := at(motion, SensorMotion, true, h.now)
		h.moves()
		h.advance(10 * time.Second)
		booted = setBackLive(t, h, booted, time.Hour)
		if live {
			h.advance(h.cfg.ExitDelay)
			h.mustBe(ModeArmed)
		} else {
			h.now = h.now.Add(h.cfg.ExitDelay)
		}
		h.e = restart(t, h)
		if as := h.e.Boot(h.now, Booted{At: booted}, []Signal{sig}); acted(as) {
			t.Fatalf("armed live %v, the movement of the exit delay: %v", live, as)
		}
		h.mustBe(ModeArmed)
	}
}

// The positive case: after the same step back, a real movement while the hub was off,
// dated on the clock as it is, before the old start of the mode, rings. It bears another
// time than the one set aside.
func TestARealMovementAfterTheClockWentBackLiveStillRings(t *testing.T) {
	h := newHarness(t)
	_, booted := movedBeforeTheMode(h)
	booted = setBackLive(t, h, booted, time.Hour)
	h.advance(5 * time.Minute)
	h.e = restart(t, h)
	moved := at(motion, SensorMotion, true, h.now.Add(-time.Minute))
	if as := h.e.Boot(h.now, Booted{At: booted}, []Signal{moved}); !acted(as) {
		t.Fatalf("a real movement while the hub was off, after the clock went back: %v", as)
	}
}

// The positive case: the movement set aside, heard back at rest then moving again (a
// rising edge), rings, live or while the hub was off, even dated before the old start of
// the mode.
func TestAMovementSetAsideThenHeardAgainRings(t *testing.T) {
	for _, live := range []bool{true, false} {
		h := newHarness(t)
		_, booted := movedBeforeTheMode(h)
		booted = setBackLive(t, h, booted, time.Hour)
		h.advance(time.Minute)
		h.signal(motion, SensorMotion, false)
		h.advance(time.Minute)
		if live {
			if as := h.moves(); !acted(as) {
				t.Fatalf("moving again live: %v", as)
			}
			continue
		}
		moved := at(motion, SensorMotion, true, h.now)
		h.advance(time.Minute)
		h.e = restart(t, h)
		if as := h.e.Boot(h.now, Booted{At: booted}, []Signal{moved}); !acted(as) {
			t.Fatalf("moving again while the hub was off: %v", as)
		}
	}
}

// A save written before the engine kept the momentary sensors' states (no "moving") is
// read without error: its Boot sets the movement from before the mode aside and keeps
// it, so the clock set back while the engine runs after it does not make it ring.
func TestASaveWithoutMovingStatesIsRead(t *testing.T) {
	h := newHarness(t)
	sig, booted := movedBeforeTheMode(h)
	b, err := json.Marshal(h.e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["moving"]; !ok {
		t.Fatal(`the save has no "moving"`)
	}
	delete(raw, "moving")
	if b, err = json.Marshal(raw); err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	h.e = restart(t, h)
	h.e.Restore(snap)
	if err := onlySiren(h.e.Boot(h.now, Booted{At: booted}, []Signal{sig})); err != nil {
		t.Fatalf("an old save: %v", err)
	}
	booted = setBackLive(t, h, booted, time.Hour)
	h.e = restart(t, h)
	if err := onlySiren(h.e.Boot(h.now, Booted{At: booted}, []Signal{sig})); err != nil {
		t.Fatalf("an old save, the clock set back since: %v", err)
	}
	h.mustBe(ModeArmed)
}
