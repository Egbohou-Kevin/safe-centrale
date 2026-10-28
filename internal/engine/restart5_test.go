package engine

import (
	"testing"
	"time"
)

// --- restarts with the clock set back --------------------------------------------

// The clock set back at a restart: a few minutes (a time saved by fake-hwclock before
// a power cut), or days (the real hub on 28 October 2026, read-only, restarted from 25
// October until NTP set it right).
var setBacks = []time.Duration{3 * time.Minute, 3 * 24 * time.Hour}

// heardThenRestarted is a sensor the engine heard before a restart, its retained state
// still the one heard: the state given back at Boot.
type heardThenRestarted struct {
	name  string
	heard func(h *harness) Signal
}

var heardBeforeTheRestart = []heardThenRestarted{
	{"a panic button pressed", func(h *harness) Signal {
		h.now = h.now.Add(time.Hour)
		pressed := h.now
		h.signal(panic_, SensorPanic, true)
		h.advance(time.Minute)
		return at(panic_, SensorPanic, true, pressed)
	}},
	{"a movement during the exit delay", func(h *harness) Signal {
		h.arm()
		h.advance(time.Minute)
		moved := h.now
		h.moves()
		h.advance(h.cfg.ExitDelay)
		h.mustBe(ModeArmed)
		h.advance(30 * time.Second)
		return at(motion, SensorMotion, true, moved)
	}},
	{"a movement that rang until the siren ran out", func(h *harness) Signal {
		h.cfg.SirenCycle, h.cfg.SirenPause, h.cfg.SirenCycles = 20*time.Second, 10*time.Second, 2
		h.e = restart(h.t, h)
		h.e.Boot(h.now, nil)
		h.armed()
		h.advance(30 * time.Second)
		moved := h.now
		h.moves()
		h.mustBe(ModeTriggered)
		h.advance(2 * time.Minute)
		h.mustBe(ModeArmed)
		return at(motion, SensorMotion, true, moved)
	}},
	{"a door closed after the broker's last save", func(h *harness) Signal {
		opened := h.now
		h.open(back)
		h.advance(30 * time.Second)
		h.close(back)
		h.armed()
		return at(back, SensorContact, true, opened)
	}},
}

// A sensor heard before a restart, its retained state still the one heard, is no news
// when the clock went back: it must not alert, ring or start a delay again. Before 28
// October 2026, pullBack dropped the times heard after now, and a mode begun after now
// made any state of a momentary sensor trusted: a panic pressed once alerted again at
// each restart, a movement already heard rang, a door closed after the broker's last
// save came back open and rang.
func TestARestartWithTheClockSetBackDoesNotActOnWhatItHeard(t *testing.T) {
	for _, d := range setBacks {
		for _, c := range heardBeforeTheRestart {
			h := newHarness(t)
			sig := c.heard(h)
			mode := h.e.Mode()
			h.e = restart(t, h)
			h.now = h.now.Add(-d)
			as := h.e.Boot(h.now, []Signal{sig})
			if err := onlySiren(as); err != nil {
				t.Errorf("%s, the clock %v back: %v", c.name, d, err)
			}
			if h.e.Mode() != mode {
				t.Errorf("%s, the clock %v back: %s became %s", c.name, d, mode, h.e.Mode())
			}

			// Nor once NTP set the clock right, at the next restart.
			h.e.Stepped(d)
			h.now = h.now.Add(d + time.Minute)
			h.e.Handle(Tick{Now: h.now})
			h.e = restart(t, h)
			if err := onlySiren(h.e.Boot(h.now, []Signal{sig})); err != nil {
				t.Errorf("%s, the clock set right: %v", c.name, err)
			}
		}
	}
}

// A press, a movement or an opening dated on the clock as it is after the restart is
// news, whatever the engine heard before the clock went back: a real intrusion while the
// hub was starting rings, a panic alerts.
func TestARestartWithTheClockSetBackActsOnWhatIsNew(t *testing.T) {
	for _, d := range setBacks {
		for _, c := range heardBeforeTheRestart {
			h := newHarness(t)
			sig := c.heard(h)
			h.e = restart(t, h)
			h.now = h.now.Add(-d)
			sig.Now = h.now.Add(-5 * time.Second)
			as := h.e.Boot(h.now, []Signal{sig})
			if len(alerts(as)) == 0 && len(eventsOf[Triggered](as)) == 0 && len(eventsOf[EntryDelayStarted](as)) == 0 {
				t.Errorf("%s again, the clock %v back: %v", c.name, d, as)
			}
		}
	}
}
