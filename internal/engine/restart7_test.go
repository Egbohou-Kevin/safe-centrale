package engine

import (
	"testing"
	"time"
)

// --- restarts with the clock set forward by NTP before the hub started ------------

// steppedBoot plays a restart where the Pi starts from back, the time the clock went
// back to, and NTP sets it right a few seconds later, before the hub starts: the hub
// starts a minute after the run before stopped, the system 30 s earlier on the clock
// set right (At), at back on the clock as it read then (Shown).
func steppedBoot(h *harness, back time.Time) Booted {
	h.now = h.now.Add(time.Minute)
	return Booted{At: h.now.Add(-30 * time.Second), Shown: back}
}

// A state dated in the seconds after the system started, on the clock as it read
// before NTP set it forward, is news: a door opened then, a press, a movement alerts or
// rings. Before 28 October 2026 it was dated before the system's start on the clock
// set right, compared with what the engine heard before the power cut, and lost. So
// too when the clock, set forward, is still behind what the engine heard (a time kept
// apart, set on a clock ahead).
func TestARestartActsOnAStateDatedBeforeNTPSetTheClockForward(t *testing.T) {
	for _, c := range keptFromBeforeTheSetBack {
		for _, behind := range []bool{false, true} {
			h := newHarness(t)
			sig, back := c.kept(h)
			h.e = restart(t, h)
			booted := steppedBoot(h, back)
			if behind {
				h.now = back.Add(time.Minute)
				booted.At = h.now.Add(-30 * time.Second)
			}
			sig.Now = back.Add(time.Second)
			if as := h.e.Boot(h.now, booted, []Signal{sig}); !acted(as) {
				t.Errorf("%s, dated before NTP set the clock forward, still behind %v: %v", c.name, behind, as)
			}
		}
	}
}

// The state the broker kept from before the power cut, dated before the time the clock
// went back to, is still no news (the correction of 28 October 2026 holds); so is one
// dated more than the uptime after it, which the clock as it read at boot had not
// reached when the hub started. Nor is the same state given again by the broker,
// bearing the very time the engine heard.
func TestASteppedClockStillTellsAStateKeptFromBeforeTheSetBack(t *testing.T) {
	for _, c := range keptFromBeforeTheSetBack {
		for _, past := range []time.Duration{0, 31 * time.Second} {
			h := newHarness(t)
			sig, back := c.kept(h)
			mode := h.e.Mode()
			h.e = restart(t, h)
			booted := steppedBoot(h, back)
			if past > 0 {
				sig.Now = back.Add(past)
			}
			if err := onlySiren(h.e.Boot(h.now, booted, []Signal{sig})); err != nil {
				t.Errorf("%s, %v past the time the clock went back to: %v", c.name, past, err)
			}
			if h.e.Mode() != mode {
				t.Errorf("%s, %v past: %s became %s", c.name, past, mode, h.e.Mode())
			}
		}
	}
	h := newHarness(t)
	h.armed()
	pressed := h.now
	h.signal(panic_, SensorPanic, true)
	h.advance(time.Minute)
	mode := h.e.Mode()
	h.e = restart(t, h)
	booted := Booted{At: h.now.Add(-30 * time.Second), Shown: pressed.Add(-time.Hour)}
	if err := onlySiren(h.e.Boot(h.now, booted, []Signal{at(panic_, SensorPanic, true, pressed)})); err != nil {
		t.Fatalf("the press the engine heard, given again: %v", err)
	}
	if h.e.Mode() != mode {
		t.Fatalf("%s became %s", mode, h.e.Mode())
	}
}

// A time the engine heard since the system started, on the clock set right, was heard
// after NTP set it forward: a state dated on the clock as it read before is older. The
// door closed by a run of the hub after the step, the broker giving back its opening
// from before, does not ring.
func TestATimeHeardSinceTheStepComparesWithAStateDatedBefore(t *testing.T) {
	h := newHarness(t)
	started := h.now // the system, on the clock set right
	h.advance(time.Minute)
	h.close(back)
	h.armed()
	h.advance(time.Minute)
	h.e = restart(t, h)
	shown := started.Add(-time.Hour)
	booted := Booted{At: started, Shown: shown}
	if err := onlySiren(h.e.Boot(h.now, booted, []Signal{at(back, SensorContact, true, shown.Add(10*time.Second))})); err != nil {
		t.Fatal(err)
	}
	h.mustBe(ModeArmed)
}

// A time read at boot that is not before the system's start on the clock set right
// (no step forward, or a wrong reading: after it, after now) changes nothing: the rule
// of the boot time alone holds, the stale state is no news and the one dated since the
// system started is.
func TestAWrongTimeReadAtBootKeepsTheRuleOfTheBootTime(t *testing.T) {
	for _, c := range keptFromBeforeTheSetBack {
		for _, shown := range []time.Duration{0, time.Second, 2 * time.Hour} {
			for _, stale := range []bool{true, false} {
				h := newHarness(t)
				sig, back := c.kept(h)
				h.e = restart(t, h)
				h.now = back
				booted := Booted{At: h.now.Add(-2 * time.Second)}
				booted.Shown = booted.At.Add(shown)
				if !stale {
					sig.Now = h.now.Add(-time.Second)
				}
				as := h.e.Boot(h.now, booted, []Signal{sig})
				if acted(as) == stale {
					t.Errorf("%s, read at boot %v after the start, stale %v: %v", c.name, shown, stale, as)
				}
			}
		}
	}
}
