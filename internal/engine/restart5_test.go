package engine

import (
	"fmt"
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
		h.e.Boot(h.now, Booted{}, nil)
		h.armed()
		h.advance(30 * time.Second)
		moved := h.now
		h.moves()
		h.mustBe(ModeTriggered)
		h.advance(2 * time.Minute)
		h.mustBe(ModeArmed)
		return at(motion, SensorMotion, true, moved)
	}},
	{"a panic button pressed again after the broker's last save", func(h *harness) Signal {
		h.now = h.now.Add(time.Hour)
		pressed := h.now
		h.signal(panic_, SensorPanic, true)
		h.advance(30 * time.Second)
		h.signal(panic_, SensorPanic, true)
		h.advance(time.Minute)
		return at(panic_, SensorPanic, true, pressed)
	}},
	// Opened a moment before it was closed, later than the clock went back to: a door
	// opened before that is told from one opened while the hub was starting by the boot
	// time only, which these tests do not give (see restart6_test.go).
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
			as := h.e.Boot(h.now, Booted{}, []Signal{sig})
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
			if err := onlySiren(h.e.Boot(h.now, Booted{}, []Signal{sig})); err != nil {
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
			as := h.e.Boot(h.now, Booted{}, []Signal{sig})
			if len(alerts(as)) == 0 && len(eventsOf[Triggered](as)) == 0 && len(eventsOf[EntryDelayStarted](as)) == 0 {
				t.Errorf("%s again, the clock %v back: %v", c.name, d, as)
			}
		}
	}
}

// The times heard before the clock went back count again once the clock catches up
// with them, whether NTP steps it or it simply runs on: at the next restart, a state
// the broker kept from before them is older, as without a clock set back. Before 28
// October 2026 (the first fix of that day), only a step told by the driver put them
// back, and only as far as the last time the engine was handed plus the step: a clock
// that ran on, or a hub quiet before the step, left them apart for good, and the stale state of a door rang at the next
// restart. A step short of a time heard leaves it apart: the clock has not caught up.
// A restart then, the state the broker kept now dated before the clock, is told from a
// state dated while the hub was starting by the boot time only, which these tests do
// not give (see restart6_test.go): not checked here.
func TestTimesHeardBeforeTheClockWentBackCountAgainOnceItCatchesUp(t *testing.T) {
	type landing struct {
		name string
		from time.Duration // from the time heard
	}
	landings := []landing{{"short of", -time.Second}, {"on", 0}, {"past", time.Minute}}
	for _, d := range setBacks {
		for _, c := range heardBeforeTheRestart {
			for _, stepped := range []bool{true, false} {
				for _, l := range landings {
					name := fmt.Sprintf("%s, the clock %v back, %s the time heard, stepped=%v", c.name, d, l.name, stepped)
					h := newHarness(t)
					sig := c.heard(h)
					mode := h.e.Mode()
					stop := h.now
					h.e = restart(t, h)
					h.now = h.now.Add(-d)
					h.e.Boot(h.now, Booted{}, []Signal{sig})
					key := sensorKey(sig.DeviceID, sig.Sensor)
					ref, apart := h.e.Snapshot().HeardAhead[key]
					if !apart {
						ref = stop // a momentary sensor: the mode began before the stop
					}
					to := ref.Add(l.from)
					if stepped {
						// A quiet hub: the clock ran on unseen before NTP stepped it.
						quiet := min(30*time.Second, to.Sub(h.now)/2)
						h.e.Stepped(to.Sub(h.now) - quiet)
					}
					h.now = to
					h.e.Handle(Tick{Now: h.now})
					if _, still := h.e.Snapshot().HeardAhead[key]; apart && still != (l.from < 0) {
						t.Errorf("%s: set apart %v", name, still)
					}
					if l.from < 0 {
						continue
					}
					h.e = restart(t, h)
					if err := onlySiren(h.e.Boot(h.now, Booted{}, []Signal{sig})); err != nil {
						t.Errorf("%s: %v", name, err)
					}
					if h.e.Mode() != mode {
						t.Errorf("%s: %s became %s", name, mode, h.e.Mode())
					}
				}
			}
		}
	}
}
