package engine

import (
	"testing"
	"time"
)

// --- restarts with the clock set back, the system's start known ------------------

// beforeTheSetBack is a sensor whose state the broker kept from before the clock went
// back, dated earlier than the time the clock went back to, and older than what the
// engine heard: the three limits written in docs/engine.md on 28 October 2026, while
// Boot did not know when the system started.
type beforeTheSetBack struct {
	name string
	// kept plays the run before the restart and returns the state the broker gives
	// back, and the time the clock goes back to, after that state and before the time
	// the engine set.
	kept func(h *harness) (Signal, time.Time)
}

var keptFromBeforeTheSetBack = []beforeTheSetBack{
	{"a door closed after the broker's last save, opened longer than the clock went back", func(h *harness) (Signal, time.Time) {
		opened := h.now
		h.open(back)
		h.advance(10 * time.Minute)
		h.close(back)
		h.armed()
		h.advance(time.Minute)
		return at(back, SensorContact, true, opened), opened.Add(5 * time.Minute)
	}},
	{"a panic button pressed twice, the broker keeping the first press", func(h *harness) (Signal, time.Time) {
		h.now = h.now.Add(time.Hour)
		first := h.now
		h.signal(panic_, SensorPanic, true)
		h.advance(10 * time.Minute)
		h.signal(panic_, SensorPanic, true)
		h.advance(time.Minute)
		return at(panic_, SensorPanic, true, first), first.Add(5 * time.Minute)
	}},
	{"a movement still active, begun before the mode", func(h *harness) (Signal, time.Time) {
		h.arm()
		h.advance(10 * time.Second)
		moved := h.now
		h.moves()
		h.advance(h.cfg.ExitDelay)
		h.mustBe(ModeArmed)
		h.advance(30 * time.Second)
		return at(motion, SensorMotion, true, moved), moved.Add(5 * time.Second)
	}},
}

// acted tells whether Boot acted on a state: an alert, an alarm, an entry delay.
func acted(as []Action) bool {
	return len(alerts(as)) > 0 || len(eventsOf[Triggered](as)) > 0 || len(eventsOf[EntryDelayStarted](as)) > 0
}

// A state the broker kept from before the clock went back, dated before the system
// started, is compared with what the engine heard: no news, it does not alert or ring.
// Before 28 October 2026 (the boot time unknown to the engine), each of these rang or
// alerted again at the restart that followed a power cut.
func TestARestartTellsAStateKeptFromBeforeTheSetBackByTheBootTime(t *testing.T) {
	for _, c := range keptFromBeforeTheSetBack {
		h := newHarness(t)
		sig, back := c.kept(h)
		mode := h.e.Mode()
		h.e = restart(t, h)
		h.now = back
		booted := h.now.Add(-2 * time.Second)
		as := h.e.Boot(h.now, booted, []Signal{sig})
		if err := onlySiren(as); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if h.e.Mode() != mode {
			t.Errorf("%s: %s became %s", c.name, mode, h.e.Mode())
		}
	}
}

// The same states dated since the system started are news: a real intrusion, a press,
// a movement while the hub was starting alerts or rings.
func TestARestartActsOnAStateDatedSinceTheSystemStarted(t *testing.T) {
	for _, c := range keptFromBeforeTheSetBack {
		h := newHarness(t)
		sig, back := c.kept(h)
		h.e = restart(t, h)
		h.now = back
		booted := h.now.Add(-2 * time.Second)
		sig.Now = h.now.Add(-time.Second)
		if as := h.e.Boot(h.now, booted, []Signal{sig}); !acted(as) {
			t.Errorf("%s, dated since the system started: %v", c.name, as)
		}
	}
}

// The boot time unknown (the uptime unreadable) or wrong (after now), Boot keeps the
// rule without it: every state dated up to now is news. An alert too many rather than
// one lost.
func TestARestartWithoutABootTimeTrustsWhatIsDatedUpToNow(t *testing.T) {
	for _, c := range keptFromBeforeTheSetBack {
		for _, booted := range []time.Duration{0, time.Second} {
			h := newHarness(t)
			sig, back := c.kept(h)
			h.e = restart(t, h)
			h.now = back
			var at time.Time
			if booted > 0 {
				at = h.now.Add(booted)
			}
			if as := h.e.Boot(h.now, at, []Signal{sig}); !acted(as) {
				t.Errorf("%s, boot time %v: %v", c.name, at, as)
			}
		}
	}
}

// A time set apart by a run before (the clock already behind it then) is not on the
// clock the broker's states were dated on during that run: the boot time does not
// order them. A door opened while the hub was down during that run, the Pi restarted
// since, rings.
func TestTheBootTimeDoesNotOrderATimeSetApartByARunBefore(t *testing.T) {
	h := newHarness(t)
	h.open(back)
	h.advance(time.Minute)
	h.close(back)
	h.armed()
	h.e = restart(t, h)
	h.now = h.now.Add(-3 * 24 * time.Hour)
	noActions(t, h.e.Boot(h.now, h.now.Add(-2*time.Second), nil))
	h.advance(time.Hour)
	opened := h.now // the hub down, the Zigbee adapter up
	h.e = restart(t, h)
	h.now = opened.Add(time.Minute)
	as := h.e.Boot(h.now, opened.Add(30*time.Second), []Signal{at(back, SensorContact, true, opened)})
	if len(eventsOf[Triggered](as)) != 1 {
		t.Fatalf("a door opened while the hub was down, its time set apart by the run before: %v", as)
	}
}
