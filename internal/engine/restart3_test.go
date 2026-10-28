package engine

import (
	"testing"
	"time"
)

// --- restarts: retained states older than what the engine knows -----------------

// at is a device's state given at Boot, dated when it began (the since of a retained
// device.state).
func at(dev string, sensor SensorKind, active bool, since time.Time) Signal {
	return Signal{Now: since, DeviceID: dev, Sensor: sensor, Active: active}
}

// After an unclean stop the broker gives back each retained state as it was at its
// last save, minutes old. A door closed after that save comes back open: it was not
// opened while the hub was off, and must not set off the alarm.
func TestARestartIgnoresAStateOlderThanWhatTheEngineHeard(t *testing.T) {
	h := newHarness(t)
	opened := h.now
	h.open(back)
	h.advance(time.Minute)
	h.close(back)
	h.armed()
	h.e = restart(t, h)
	h.now = h.now.Add(10 * time.Minute)
	noActions(t, h.e.Boot(h.now, time.Time{}, []Signal{at(back, SensorContact, true, opened)}))
	h.mustBe(ModeArmed)
	if h.e.Snapshot().Open[back] {
		t.Fatal("the back door is closed, as the engine heard last")
	}

	// A door opened while the hub was off is news.
	h.e = restart(t, h)
	as := h.e.Boot(h.now, time.Time{}, []Signal{at(back, SensorContact, true, h.now.Add(-time.Minute))})
	if tr := onlyEvent[Triggered](t, as); tr.Cause.DeviceID != back {
		t.Fatalf("a door opened during the outage rings: %+v", tr)
	}

	// The other way round: a door opened just before the stop, whose retained state is
	// still "closed", stays open for the engine, and blocks the arming.
	h = newHarness(t)
	h.open(back)
	h.e = restart(t, h)
	h.e.Boot(h.now, time.Time{}, []Signal{at(back, SensorContact, false, t0.Add(-time.Hour))})
	if r := replyOf(t, h.arm()); r.Reason != ReasonZoneOpen {
		t.Fatalf("the back door is still open: %+v", r)
	}
}

// A smoke detector that cleared after the broker's last save comes back in smoke: it is
// not a new fire. A movement seen during the exit delay, whose return to rest the
// broker lost, is no intrusion once armed.
func TestARestartIgnoresAnAlarmStateTheEngineAlreadyHad(t *testing.T) {
	h := newHarness(t)
	h.advance(time.Minute)
	went := h.now
	h.signal(smoke, SensorSmoke, true)
	h.advance(time.Minute)
	h.signal(smoke, SensorSmoke, false)
	h.cmd(CmdSilence, goodCode("u_1"))
	h.e = restart(t, h)
	noActions(t, h.e.Boot(h.now, time.Time{}, []Signal{at(smoke, SensorSmoke, true, went)}))
	h.mustBe(ModeDisarmed)

	h = newHarness(t)
	h.arm()
	h.advance(time.Minute)
	moved := h.now
	h.moves()
	h.advance(h.cfg.ExitDelay)
	h.mustBe(ModeArmed)
	h.e = restart(t, h)
	noActions(t, h.e.Boot(h.now, time.Time{}, []Signal{at(motion, SensorMotion, true, moved)}))
	h.mustBe(ModeArmed)

	// The same movement, the hub off before the exit delay ended.
	h = newHarness(t)
	h.arm()
	h.advance(time.Minute)
	h.moves()
	h.e = restart(t, h)
	h.now = h.now.Add(10 * time.Minute)
	h.e.Boot(h.now, time.Time{}, []Signal{at(motion, SensorMotion, true, t0.Add(time.Minute))})
	h.mustBe(ModeArmed)

	// A movement after the arming, while the hub was off, is an intrusion.
	h.e = restart(t, h)
	if as := h.e.Boot(h.now, time.Time{}, []Signal{at(motion, SensorMotion, true, h.now.Add(-time.Minute))}); len(eventsOf[Triggered](as)) != 1 {
		t.Fatalf("a movement during the outage rings: %v", as)
	}
}

// A door opened between the restart and Boot is applied to the saved state: there, an
// intrusion whose siren ran out while the hub was off drops it as the alarm already
// ringing. Boot must still take it for the opening it is, once that alarm is over.
func TestADoorOpenedBeforeBootIsNotKnownAlready(t *testing.T) {
	for _, c := range []struct {
		dev  string
		want func(t *testing.T, as []Action)
	}{
		{back, func(t *testing.T, as []Action) { onlyEvent[Triggered](t, as) }},
		{door, func(t *testing.T, as []Action) { onlyEvent[EntryDelayStarted](t, as) }},
	} {
		h := newHarness(t)
		h.armed()
		h.breakWindow()
		h.e = restart(t, h)
		h.now = h.now.Add(time.Hour)
		noActions(t, h.open(c.dev))
		h.now = h.now.Add(time.Second)
		c.want(t, h.e.Boot(h.now, time.Time{}, []Signal{at(c.dev, SensorContact, true, h.now.Add(-time.Second))}))
	}
}
