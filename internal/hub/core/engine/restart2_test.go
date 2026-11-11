package engine

import (
	"testing"
	"time"
)

// --- restarts: what was known before, and a configuration that changed -------

// A new configuration is applied by a restart: the automatic arming saved before it
// is checked against the new closing times, not run as it was planned.
func TestARestartPlansAgainForChangedClosingTimes(t *testing.T) {
	// The closing removed (staying open late): no arming at the old time.
	h := newHarness(t, closes22)
	h.cfg.Closing = nil
	h.e = restart(t, h)
	h.e.Boot(h.now, Booted{}, nil)
	h.mustPlan(time.Time{})
	h.advance(61 * time.Minute) // 22:01
	h.mustBe(ModeDisarmed)

	// Moved earlier: the shop arms at the new time, not at the old one.
	h = newHarness(t, closes22)
	h.cfg.Closing = allWeek(21, 30)
	h.e = restart(t, h)
	h.e.Boot(h.now, Booted{}, nil)
	h.mustPlan(t0.Add(30 * time.Minute))
	h.advance(31 * time.Minute) // 21:31
	h.mustBe(ModeArming)

	// Unchanged: the postponements are kept, under the new limit.
	h = newHarness(t, closes22)
	h.postpone()
	h.postpone() // 23:00
	h.cfg.AutoArmLimit = 30 * time.Minute
	h.e = restart(t, h)
	h.e.Boot(h.now, Booted{}, nil)
	h.mustPlan(closing.Add(30 * time.Minute))
	if s := h.e.Snapshot().Schedule; s.Postponed != 2 || !s.Limit.Equal(closing.Add(30*time.Minute)) {
		t.Fatalf("the plan keeps its postponements under the new limit: %+v", s)
	}
}

// A plan saved before the plan named its closing is still recognised.
func TestARestartKeepsAnOldPlanWithoutItsClosing(t *testing.T) {
	h := newHarness(t, closes22)
	h.postpone()
	snap := h.e.Snapshot()
	snap.Schedule.Closing = time.Time{}
	h.e, _ = New(h.cfg, func() string { return "inc" }, h.now)
	h.e.Restore(snap)
	h.e.Boot(h.now, Booted{}, nil)
	h.mustPlan(closing.Add(30 * time.Minute))
}

// An interior door may be open at arming (it blocks nothing). After a restart while
// armed it is still open, as it was: that is not an opening, and nothing rings.
func TestARestartDoesNotTakeAContactKnownOpenForAnOpening(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Zones["dev_office"] = Zone{Name: "office", Kind: ZoneInterior} })
	h.open("dev_office")
	h.armed()
	h.e = restart(t, h)
	noActions(t, h.e.Boot(h.now, Booted{}, []Signal{{DeviceID: "dev_office", Sensor: SensorContact, Active: true}}))
	h.mustBe(ModeArmed)

	// The entry door forced, the siren silenced with a code: armed again, the door
	// still open. A restart starts no second entry delay.
	h = newHarness(t)
	h.armed()
	h.open(door)
	h.advance(h.cfg.EntryDelay)
	h.cmd(CmdSilence, goodCode("u_1"))
	h.mustBe(ModeArmed)
	h.e = restart(t, h)
	noActions(t, h.e.Boot(h.now, Booted{}, []Signal{{DeviceID: door, Sensor: SensorContact, Active: true}}))
	h.mustBe(ModeArmed)
	h.advance(time.Minute)
	h.mustBe(ModeArmed)
}

// The entry door opened during the exit delay and left open, the hub off longer than
// the whole siren program: the replay rings one alarm, and the door that never moved
// does not raise a second one.
func TestALongOutageAfterTheExitDelayEndsInOneAlarm(t *testing.T) {
	for _, off := range []time.Duration{10 * time.Minute, time.Hour} {
		h := newHarness(t)
		h.arm()
		h.open(door)
		h.e = restart(t, h)
		h.now = h.now.Add(off)
		as := h.e.Boot(h.now, Booted{}, []Signal{{DeviceID: door, Sensor: SensorContact, Active: true}})
		as = append(as, h.advance(5*time.Minute)...)
		if n := len(eventsOf[Triggered](as)); n != 1 {
			t.Fatalf("off %v: %d alarms, want 1: %v", off, n, as)
		}
		if n := len(eventsOf[EntryDelayStarted](as)); n != 1 {
			t.Fatalf("off %v: %d entry delays, want 1", off, n)
		}
	}
}

// A clock that ran ahead when a delay or a siren step was set, then corrected before
// a restart: the step lasts no longer than its own length from now.
func TestARestartPullsBackDeadlinesSetByAClockAhead(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.now = h.now.Add(time.Hour) // ahead
	h.breakWindow()
	h.e = restart(t, h)
	h.now = h.now.Add(-time.Hour) // corrected
	as := h.e.Boot(h.now, Booted{}, nil)
	for _, a := range as {
		if on, ok := a.(SirenOn); ok && on.Duration > h.cfg.SirenCycle {
			t.Fatalf("the siren told to sound %v, more than a cycle", on.Duration)
		}
	}
	if s := h.e.State(h.now); s.EndsAt.After(h.now.Add(h.cfg.SirenCycle)) {
		t.Fatalf("the cycle ends at %v", s.EndsAt)
	}

	h = newHarness(t)
	h.now = h.now.Add(time.Hour)
	h.arm()
	h.e = restart(t, h)
	h.now = h.now.Add(-time.Hour)
	h.e.Boot(h.now, Booted{}, nil)
	h.advance(h.cfg.ExitDelay)
	h.mustBe(ModeArmed)
}
