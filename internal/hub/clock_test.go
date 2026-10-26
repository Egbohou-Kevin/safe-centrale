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
