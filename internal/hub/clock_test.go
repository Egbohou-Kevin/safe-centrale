package hub

import (
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/engine"
)

// The Pi has no battery clock: after a power cut it starts from the last time it knew,
// hours late, and NTP sets it forward a few seconds later. These tests play that step
// (FakeClock.Set, which the monotonic clock does not see) in the middle of what the hub
// times: each keeps the time that really passed, and what is published is dated on the
// new time.

// An exit delay under way does not arm at once, and an entry delay under way does not
// ring at once: each lasts its whole length.
func TestAClockSetForwardShortensNoDelay(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.keypad(contract.ValArm, "")
	r.mustBe(engine.ModeArming)
	start := r.clock.Now()
	r.clock.Set(start.Add(14 * time.Hour)) // NTP
	r.clock.Advance(time.Minute)
	r.settle()
	if m := r.mode(); m != engine.ModeArming {
		t.Fatalf("a minute into the exit delay, after a clock set forward: %s", m)
	}
	r.clock.Advance(time.Minute)
	r.mustBe(engine.ModeArmed)
	if armed := eventsOf(r, contract.TypeAlarmArmed); len(armed) != 1 || !armed[0].Env.TS.Equal(start.Add(14*time.Hour+2*time.Minute)) {
		t.Fatalf("alarm.armed: %+v", armed)
	}

	r.device(devDoor, contract.CapContact, "open")
	r.mustBe(engine.ModeEntryDelay)
	r.clock.Set(r.clock.Now().Add(3 * time.Hour))
	r.clock.Advance(30 * time.Second)
	r.settle()
	if m := r.mode(); m != engine.ModeEntryDelay {
		t.Fatalf("30 s into the entry delay, after a clock set forward: %s", m)
	}
	r.clock.Advance(15 * time.Second)
	r.mustBe(engine.ModeTriggered)
	r.validateAll()
}

// A Zigbee adapter and a GSM gateway heard just before the step are not silent for
// hours: no zigbee_down, no gsm_degraded. One that stays silent is still told.
func TestAClockSetForwardRaisesNoSilence(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	line := contract.GSMLine{ID: "line1", Responding: true, Registered: true, Signal: 70, Credit: "ok"}
	r.send(contract.GSMStatusTopic(site), contract.TypeGSMStatus, gsmStatus(line))
	r.send(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus,
		contract.ZigbeeStatusData{Bridge: contract.BridgeOnline, Devices: 4, Offline: []string{}})
	r.settle()
	faults := func() (out []string) {
		for _, m := range eventsOf(r, contract.TypeAlertRequest) {
			if k := dataOf[contract.AlertRequestData](t, m).Kind; k == "zigbee_down" || k == "gsm_degraded" {
				out = append(out, k)
			}
		}
		return out
	}
	r.clock.Set(r.clock.Now().Add(14 * time.Hour)) // NTP
	r.clock.Advance(30 * time.Second)
	agentStatus(r, contract.UplinkUp) // any news: the monitor looks again
	r.settle()
	if f := faults(); len(f) != 0 {
		t.Fatalf("alerts on a clock set forward: %v", f)
	}
	health := r.broker.On(contract.HealthTopic(site))
	if d := dataOf[contract.HealthData](t, health[len(health)-1]); d.Links["zigbee"] != "ok" || d.Links["gsm"] != "ok" {
		t.Fatalf("links after a clock set forward: %+v", d.Links)
	}
	// Silent from the step on: the Zigbee adapter is told after 3 minutes of silence and
	// 3 of outage, counted from when it was really last heard.
	for range 6 {
		r.clock.Advance(time.Minute)
		agentStatus(r, contract.UplinkUp)
		r.send(contract.GSMStatusTopic(site), contract.TypeGSMStatus, gsmStatus(line))
		r.settle()
	}
	drivertest.WaitFor(t, "zigbee_down", func() bool { r.settle(); f := faults(); return len(f) == 1 && f[0] == "zigbee_down" })
	r.validateAll()
}

// The waves and reminders of an incident under way do not all go at once: the second
// wave leaves two minutes after the alarm, counted on the time that really passed, and
// every request still says when the alarm was raised.
func TestAClockSetForwardSendsNoWaveEarly(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	trigger(r)
	raised := r.clock.Now()
	waves := func() (most int) {
		for _, m := range eventsOf(r, contract.TypeAlertRequest) {
			d := dataOf[contract.AlertRequestData](t, m)
			if !d.IncidentAt.Equal(raised) {
				t.Fatalf("incident_at %v, want %v", d.IncidentAt.Time, raised)
			}
			most = max(most, d.Wave)
		}
		return most
	}
	if w := waves(); w != 1 {
		t.Fatalf("first wave: %d", w)
	}
	r.clock.Set(raised.Add(14 * time.Hour)) // NTP
	r.clock.Advance(time.Minute)
	r.settle()
	if w := waves(); w != 1 {
		t.Fatalf("a minute after the alarm, after a clock set forward: wave %d", w)
	}
	r.clock.Advance(time.Minute)
	drivertest.WaitFor(t, "the second wave", func() bool { r.settle(); return waves() == 2 })
	for _, m := range eventsOf(r, contract.TypeAlertRequest) {
		if dataOf[contract.AlertRequestData](t, m).Wave == 2 && !m.Env.TS.Equal(raised.Add(14*time.Hour+2*time.Minute)) {
			t.Fatalf("second wave asked at %v", m.Env.TS.Time)
		}
	}
	r.validateAll()
}

// A power cut while armed: the Pi starts again from the last time it saved, minutes
// behind, and the broker gives back the back door open, its closing lost with its last
// save. Opened longer ago than the clock went back, that state is dated before the
// system started: no intrusion. One dated since, while the hub was starting, rings.
// The uptime unknown, or putting the system's start after the hub's (a wrong reading),
// the hub keeps the rule without it: the stale door rings, an alarm too many rather
// than one lost.
func TestARestartWithTheClockSetBackTellsAStaleDoorByTheBootTime(t *testing.T) {
	const window = 50 * time.Millisecond
	cases := []struct {
		name   string
		uptime func() (time.Duration, bool)
		since  time.Duration // from when the clock went back to
		rings  bool
	}{
		{"stale, the uptime known", func() (time.Duration, bool) { return 20 * time.Second, true }, -5 * time.Minute, false},
		{"opened while the hub was starting", func() (time.Duration, bool) { return 20 * time.Second, true }, -5 * time.Second, true},
		{"stale, the uptime unknown", func() (time.Duration, bool) { return 0, false }, -5 * time.Minute, true},
		{"stale, no uptime", nil, -5 * time.Minute, true},
		{"stale, an uptime shorter than the hub's run", func() (time.Duration, bool) { return window / 2, true }, -5 * time.Minute, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first := newRig(t, nil)
			first.start()
			first.device(devBack, contract.CapContact, "open")
			first.clock.Advance(10 * time.Minute)
			first.device(devBack, contract.CapContact, "closed")
			first.keypad(contract.ValArm, "")
			first.keypad(contract.ValArmNow, "")
			first.mustBe(engine.ModeArmed)
			first.stop()

			back := t0.Add(5 * time.Minute) // after the opening, before the closing
			second := newRigWith(t, back, first.store, func(d *Deps) { d.Uptime = c.uptime },
				func(c *Config) { c.BootWindow = window })
			open := event(devBack, contract.CapContact, "open")
			open.Since = contract.TSPtr(back.Add(c.since))
			second.send(contract.DeviceStateTopic(site, devBack, contract.CapContact), contract.TypeDeviceState, open)
			second.start()
			second.settle()
			if got := len(eventsOf(second, contract.TypeAlarmTriggered)) == 1; got != c.rings {
				t.Fatalf("alarm %v, want %v", got, c.rings)
			}
		})
	}
}

// A power cut while armed, NTP setting the clock forward before the hub starts: the
// Pi started from back, five minutes behind, and the back door opened in its first
// seconds, dated on that clock, rings (before 28 October 2026 it was dated before the
// system's start on the clock set right and lost). Its state from before the power
// cut, dated before back or more than the uptime after it, does not. The time read at
// boot unknown, unreadable or not before the system's start (after it, after now), the
// rule of the boot time alone holds: the stale door does not ring, one dated since the
// system started does.
func TestARestartAfterNTPSetTheClockForwardTellsADoorOpenedMeanwhile(t *testing.T) {
	const window = 50 * time.Millisecond
	back := t0.Add(5 * time.Minute) // after the opening, before the closing
	known := func(at time.Time) func() (time.Time, bool) { return func() (time.Time, bool) { return at, true } }
	cases := []struct {
		name  string
		shown func(start time.Time) func() (time.Time, bool)
		since func(start time.Time) time.Time
		rings bool
	}{
		{"opened before the step", func(time.Time) func() (time.Time, bool) { return known(back) },
			func(time.Time) time.Time { return back.Add(5 * time.Second) }, true},
		{"stale, before back", func(time.Time) func() (time.Time, bool) { return known(back) },
			func(time.Time) time.Time { return t0 }, false},
		{"stale, more than the uptime after back", func(time.Time) func() (time.Time, bool) { return known(back) },
			func(time.Time) time.Time { return back.Add(time.Minute) }, false},
		{"stale, unknown", func(time.Time) func() (time.Time, bool) { return nil },
			func(time.Time) time.Time { return t0 }, false},
		{"stale, unreadable", func(time.Time) func() (time.Time, bool) { return func() (time.Time, bool) { return back, false } },
			func(time.Time) time.Time { return t0 }, false},
		{"stale, read after the start", known, func(time.Time) time.Time { return t0 }, false},
		{"stale, read after now", func(start time.Time) func() (time.Time, bool) { return known(start.Add(time.Hour)) },
			func(time.Time) time.Time { return t0 }, false},
		{"since the start, unknown", func(time.Time) func() (time.Time, bool) { return nil },
			func(start time.Time) time.Time { return start.Add(-5 * time.Second) }, true},
		{"since the start, read after now", func(start time.Time) func() (time.Time, bool) { return known(start.Add(time.Hour)) },
			func(start time.Time) time.Time { return start.Add(-5 * time.Second) }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first := newRig(t, nil)
			first.start()
			first.device(devBack, contract.CapContact, "open")
			first.clock.Advance(10 * time.Minute)
			first.device(devBack, contract.CapContact, "closed")
			first.keypad(contract.ValArm, "")
			first.keypad(contract.ValArmNow, "")
			first.mustBe(engine.ModeArmed)
			first.stop()

			start := first.clock.Now().Add(time.Minute) // the clock set right
			second := newRigWith(t, start, first.store, func(d *Deps) {
				d.Uptime = func() (time.Duration, bool) { return 20 * time.Second, true }
				d.StartShown = c.shown(start)
			}, func(c *Config) { c.BootWindow = window })
			open := event(devBack, contract.CapContact, "open")
			open.Since = contract.TSPtr(c.since(start))
			second.send(contract.DeviceStateTopic(site, devBack, contract.CapContact), contract.TypeDeviceState, open)
			second.start()
			second.settle()
			if got := len(eventsOf(second, contract.TypeAlarmTriggered)) == 1; got != c.rings {
				t.Fatalf("alarm %v, want %v", got, c.rings)
			}
		})
	}
}

// The hub restarts within a system run that NTP set forward 14 h while the hub ran
// (internet back hours after the boot), the trace of the start telling no step at boot:
// the movement heard during the exit delay, still active and kept by the broker, does
// not ring again. Before 28 October 2026 the hub gave the trace only when it told a
// step: the first Boot kept none, and the same trace, read again after the live step,
// took every state heard before it for one dated before NTP set the clock at boot.
// The positive case, a new system run whose trace tells a step, is
// TestARestartAfterNTPSetTheClockForwardTellsADoorOpenedMeanwhile.
func TestARestartAfterALiveStepDoesNotRingAMovementHeardBefore(t *testing.T) {
	const window = 50 * time.Millisecond
	const up = 20 * time.Minute // the system's uptime at the hub's first start
	trace := func() (time.Time, bool) { return t0.Add(-up), true }
	first := newRigWith(t, t0, nil, func(d *Deps) {
		d.Uptime = func() (time.Duration, bool) { return up, true }
		d.StartShown = trace
	})
	first.start()
	first.keypad(contract.ValArm, "")
	first.mustBe(engine.ModeArming)
	first.clock.Advance(10 * time.Second)
	heard := first.clock.Now()
	first.device(devMotion, contract.CapMotion, "detected")
	first.settle()
	first.clock.Advance(2 * time.Minute)
	first.mustBe(engine.ModeArmed)
	first.clock.Set(first.clock.Now().Add(14 * time.Hour)) // NTP, live
	first.clock.Advance(time.Minute)
	first.settle()
	first.mustBe(engine.ModeArmed)
	first.stop()

	ran := first.clock.Now().Sub(t0) - 14*time.Hour // the time that really passed
	second := newRigWith(t, first.clock.Now(), first.store, func(d *Deps) {
		d.Uptime = func() (time.Duration, bool) { return up + ran, true }
		d.StartShown = trace
	}, func(c *Config) { c.BootWindow = window })
	moved := event(devMotion, contract.CapMotion, "detected")
	moved.Since = contract.TSPtr(heard)
	second.send(contract.DeviceStateTopic(site, devMotion, contract.CapMotion), contract.TypeDeviceState, moved)
	second.start()
	second.settle()
	if got := eventsOf(second, contract.TypeAlarmTriggered); len(got) != 0 {
		t.Fatalf("the movement heard before the live step rang again: %d alarm.triggered", len(got))
	}
	second.mustBe(engine.ModeArmed)
}

// The clock set back an hour while the hub runs, the site armed, a movement heard live
// during the exit delay and still active does not ring at the next restart. The broker
// gives it back dated by the adapter (its since), a few milliseconds before the hub's
// loop handed it over: the engine set it aside by the loop's time, and until the review
// of 28 October 2026 recognized only that very time, so the hub rang (see
// engine.liveLag).
func TestARestartAfterTheClockWentBackLiveDoesNotRingAMovementHeardBefore(t *testing.T) {
	const window = 50 * time.Millisecond
	const up = 20 * time.Minute // the system's uptime at the hub's first start
	first := newRigWith(t, t0, nil, func(d *Deps) {
		d.Uptime = func() (time.Duration, bool) { return up, true }
	})
	first.start()
	first.keypad(contract.ValArm, "")
	first.mustBe(engine.ModeArming)
	first.clock.Advance(10 * time.Second)
	dated := first.clock.Now().Add(-7 * time.Millisecond) // by the adapter, before the hub heard it
	first.device(devMotion, contract.CapMotion, "detected")
	first.settle()
	first.clock.Advance(2 * time.Minute)
	first.mustBe(engine.ModeArmed)
	first.clock.Set(first.clock.Now().Add(-time.Hour)) // the clock set back, live
	first.clock.Advance(time.Minute)
	first.device(devBack, contract.CapContact, "closed") // the engine is handed the clock as it is
	first.settle()
	first.mustBe(engine.ModeArmed)
	first.stop()

	ran := first.clock.Now().Sub(t0) + time.Hour // the time that really passed
	second := newRigWith(t, first.clock.Now(), first.store, func(d *Deps) {
		d.Uptime = func() (time.Duration, bool) { return up + ran, true }
	}, func(c *Config) { c.BootWindow = window })
	moved := event(devMotion, contract.CapMotion, "detected")
	moved.Since = contract.TSPtr(dated)
	second.send(contract.DeviceStateTopic(site, devMotion, contract.CapMotion), contract.TypeDeviceState, moved)
	second.start()
	second.settle()
	if got := eventsOf(second, contract.TypeAlarmTriggered); len(got) != 0 {
		t.Fatalf("the movement heard live before the clock went back rang: %d alarm.triggered", len(got))
	}
	second.mustBe(engine.ModeArmed)
}
