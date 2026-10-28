package engine

import (
	"testing"
	"time"
)

// A panic button pressed before the arming, its retained state still "pressed", is
// no news to a restart during the exit delay: it alerted when it was pressed. Found
// by the random sequences on 28 October 2026: during an exit delay still running,
// every retained state of a momentary sensor was trusted, its end being after now,
// and a panic that sounds the siren rang again.
func TestARestartDuringTheExitDelayDoesNotPressThePanicAgain(t *testing.T) {
	for _, siren := range []bool{false, true} {
		h := newHarness(t, func(c *Config) { c.PanicSiren = siren })
		pressed := h.now
		if as := h.signal(panic_, SensorPanic, true); len(alerts(as)) != 1 {
			t.Fatalf("the panic, pressed: %v", as)
		}
		h.advance(time.Minute)
		h.arm()
		if siren {
			h.cmd(CmdDisarm, goodCode("u_owner"))
			h.arm()
		}
		h.advance(30 * time.Second)
		h.e = restart(t, h)
		h.now = h.now.Add(10 * time.Second)
		noActions(t, h.e.Boot(h.now, []Signal{at(panic_, SensorPanic, true, pressed)}))
		h.mustBe(ModeArming)
	}
}

// Nor is a panic pressed in the mode the engine is still in: it alerted when it was
// pressed, and a restart must not alert it again, under a new incident, each time. Its
// release may not have been saved by the broker yet (see older). The engine keeps when
// it heard the button, as it does for a door. A press while the hub was off is news.
// Found by the random sequences on 28 October 2026.
func TestARestartDoesNotPressThePanicAgain(t *testing.T) {
	h := newHarness(t)
	h.now = h.now.Add(time.Hour)
	pressed := h.now
	h.signal(panic_, SensorPanic, true)
	h.advance(time.Minute)
	h.e = restart(t, h)
	noActions(t, h.e.Boot(h.now, []Signal{at(panic_, SensorPanic, true, pressed)}))

	h.e = restart(t, h)
	h.now = h.now.Add(time.Minute)
	if as := h.e.Boot(h.now, []Signal{at(panic_, SensorPanic, true, h.now.Add(-time.Second))}); len(alerts(as)) != 1 {
		t.Fatalf("a press while the hub was off: %v", as)
	}
}
