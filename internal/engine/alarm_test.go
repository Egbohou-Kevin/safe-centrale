package engine

import (
	"testing"
	"time"
)

// --- entry and alarm -----------------------------------------------------

func TestEntryDelayThenDisarmInTime(t *testing.T) {
	h := newHarness(t)
	h.armed()

	as := h.open(door)
	h.mustBe(ModeEntryDelay)
	ev := onlyEvent[EntryDelayStarted](t, as)
	if ev.DeviceID != door || !ev.EndsAt.Equal(h.now.Add(45*time.Second)) {
		t.Fatalf("entry delay: %+v", ev)
	}

	h.advance(40 * time.Second)
	h.mustBe(ModeEntryDelay)
	as = h.cmd(CmdDisarm, goodCode("u_guard"))
	h.mustBe(ModeDisarmed)
	if len(alerts(as)) != 0 || count[SirenOn](as) != 0 {
		t.Fatalf("disarmed in time: no siren and no alert, got %v", as)
	}
	noActions(t, h.advance(10*time.Minute))
}

func TestEntryDelayExpiresAndTriggers(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.open(door)

	as := h.advance(45 * time.Second)
	h.mustBe(ModeTriggered)
	tr := onlyEvent[Triggered](t, as)
	if tr.Cause.DeviceID != door || tr.Cause.Sensor != SensorContact || tr.Cause.Zone != "entrance" || tr.Incident == "" {
		t.Fatalf("cause of the alarm: %+v", tr)
	}
	if count[SirenOn](as) != 1 {
		t.Fatalf("the siren must start: %v", as)
	}
	al := alerts(as)
	if len(al) != 1 || al[0].Kind != AlertIntrusion || al[0].Urgency != UrgencyHigh || al[0].Incident != tr.Incident {
		t.Fatalf("alert: %+v", al)
	}
	cy := onlyEvent[SirenCycleStarted](t, as)
	if cy.Cycle != 1 || cy.Of != 3 || cy.Incident != tr.Incident {
		t.Fatalf("cycle: %+v", cy)
	}
}

func TestLateTickStillTriggersAtDeadline(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.open(door)
	// The tick is very late: the alarm is replayed at its own time (45 s), so the
	// siren has already run a whole cycle and started its pause. Starting it now would
	// be wrong: it stays off.
	as := h.advance(5*time.Minute + 45*time.Second + time.Second)
	if count[SirenOn](as) != 0 || count[SirenOff](as) != 1 || len(eventsOf[Triggered](as)) != 1 {
		t.Fatalf("want the alarm replayed with the siren off, got %v", as)
	}
	h.mustBe(ModeTriggered)
	if got := h.e.Snapshot().Incident.Cycle; got != 1 {
		t.Fatalf("cycle %d", got)
	}
}

func TestInstantZoneTriggersImmediately(t *testing.T) {
	for _, tc := range []struct {
		dev    string
		sensor SensorKind
	}{
		{window, SensorGlassBreak},
		{window, SensorVibration},
		{back, SensorContact},
	} {
		t.Run(tc.dev+"/"+string(tc.sensor), func(t *testing.T) {
			h := newHarness(t)
			h.armed()
			as := h.signal(tc.dev, tc.sensor, true)
			h.mustBe(ModeTriggered)
			if count[SirenOn](as) != 1 || len(alerts(as)) != 1 {
				t.Fatalf("want siren and alert: %v", as)
			}
		})
	}
}

func TestInteriorMotionTriggersWhenArmed(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.moves()
	h.mustBe(ModeTriggered)
}

func TestMotionIgnoredDuringEntryDelay(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.open(door)
	noActions(t, h.moves()) // the person coming in walks to the keypad
	h.mustBe(ModeEntryDelay)
}

func TestSecondDoorOpeningDoesNotRestartEntryDelay(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.open(door)
	h.advance(30 * time.Second)
	h.close(door)
	noActions(t, h.open(door))
	h.advance(15 * time.Second) // 45 s since the first opening
	h.mustBe(ModeTriggered)
}

func TestInstantZoneDuringEntryDelayTriggers(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.open(door)
	h.breakWindow()
	h.mustBe(ModeTriggered)
	if h.e.Snapshot().EntryCause != nil {
		t.Fatal("the entry cause must be cleared")
	}
	noActions(t, h.advance(0))
}

func TestNothingWatchesWhenDisarmed(t *testing.T) {
	h := newHarness(t)
	noActions(t, h.open(door))
	noActions(t, h.open(back))
	noActions(t, h.moves())
	noActions(t, h.breakWindow())
	h.mustBe(ModeDisarmed)
}

func TestUnknownDeviceAndInactiveSignalsAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.armed()
	noActions(t, h.signal("dev_unknown", SensorContact, true))
	noActions(t, h.close(door))
	noActions(t, h.signal(motion, SensorMotion, false))
	h.mustBe(ModeArmed)
}

// --- siren: cycles, resumption, end -------------------------------------------

func TestSirenCyclesThenBackToArmed(t *testing.T) {
	h := newHarness(t)
	h.armed()
	start := h.now
	as := h.breakWindow()
	inc := onlyEvent[Triggered](t, as).Incident

	type step struct {
		after time.Duration // since the alarm
		on    int
		off   int
		cycle int // cycle number announced, 0 if none
	}
	// 5 min of siren, 2 min pause, three cycles.
	steps := []step{
		{5 * time.Minute, 0, 1, 0},  // end of cycle 1
		{7 * time.Minute, 1, 0, 2},  // start of cycle 2
		{12 * time.Minute, 0, 1, 0}, // end of cycle 2
		{14 * time.Minute, 1, 0, 3}, // start of cycle 3
		{19 * time.Minute, 0, 1, 0}, // end of cycle 3: incident over
	}
	for i, s := range steps {
		h.now = start.Add(s.after - time.Second)
		noActions(t, h.e.Handle(Tick{Now: h.now})) // one second before: nothing
		h.now = start.Add(s.after)
		as := h.e.Handle(Tick{Now: h.now})
		if count[SirenOn](as) != s.on || count[SirenOff](as) != s.off {
			t.Fatalf("step %d (%v): siren on=%d off=%d, got %v", i, s.after, s.on, s.off, as)
		}
		if s.cycle > 0 {
			cy := onlyEvent[SirenCycleStarted](t, as)
			if cy.Cycle != s.cycle || cy.Incident != inc {
				t.Fatalf("step %d: cycle %+v", i, cy)
			}
		}
	}

	h.mustBe(ModeArmed)
	snap := h.e.Snapshot()
	if snap.Incident != nil || !snap.Deadline.IsZero() {
		t.Fatalf("the incident must be closed: %+v", snap)
	}
	if snap.LastTrigger == nil || snap.LastTrigger.DeviceID != window {
		t.Fatalf("memory of the alarm lost: %+v", snap.LastTrigger)
	}

	// Armed again: a new signal makes a new incident.
	as = h.moves()
	if got := onlyEvent[Triggered](t, as).Incident; got == inc {
		t.Fatal("a new incident needs a new identifier")
	}
}

func TestClearedByTimeout(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	var cleared []Cleared
	for _, d := range []time.Duration{5 * time.Minute, 2 * time.Minute, 5 * time.Minute, 2 * time.Minute, 5 * time.Minute} {
		cleared = append(cleared, eventsOf[Cleared](h.advance(d))...)
	}
	if len(cleared) != 1 || cleared[0].By != ClearedByTimeout || cleared[0].Duration != 19*time.Minute {
		t.Fatalf("cleared: %+v", cleared)
	}
}

func TestDisarmWhileTriggeredStopsEverything(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	h.advance(90 * time.Second)

	as := h.cmd(CmdDisarm, goodCode("u_owner"), func(c *Command) { c.Origin = OriginApp })
	h.mustBe(ModeDisarmed)
	if count[SirenOff](as) != 1 {
		t.Fatalf("the siren must stop: %v", as)
	}
	cl := onlyEvent[Cleared](t, as)
	if cl.By != ClearedByUser || cl.User != "u_owner" || cl.Duration != 90*time.Second {
		t.Fatalf("cleared: %+v", cl)
	}
	onlyEvent[Disarmed](t, as)
	noActions(t, h.advance(time.Hour)) // no cycle resumes
}

func TestSilenceKeepsSystemArmed(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()

	as := h.cmd(CmdSilence, goodCode("u_guard"))
	h.mustBe(ModeArmed)
	if count[SirenOff](as) != 1 || len(eventsOf[Cleared](as)) != 1 || len(eventsOf[Disarmed](as)) != 0 {
		t.Fatalf("silence: %v", as)
	}
	noActions(t, h.advance(time.Hour))

	// The zones still watch.
	h.moves()
	h.mustBe(ModeTriggered)
}

func TestSilenceRefusedWhenNotTriggered(t *testing.T) {
	h := newHarness(t)
	h.armed()
	r := replyOf(t, h.cmd(CmdSilence, goodCode("u_guard")))
	if r.Status != StatusRefused || r.Reason != ReasonWrongState {
		t.Fatalf("reply %+v", r)
	}
}

// --- permanent zones ------------------------------------------------------

func TestSmokeTriggersInEveryMode(t *testing.T) {
	for _, mode := range []string{"disarmed", "arming", "armed"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			switch mode {
			case "arming":
				h.arm()
			case "armed":
				h.armed()
			}
			as := h.signal(smoke, SensorSmoke, true)
			h.mustBe(ModeTriggered)
			al := alerts(as)
			if len(al) != 1 || al[0].Kind != AlertFire || al[0].Urgency != UrgencyCritical {
				t.Fatalf("fire alert: %+v", al)
			}
			if count[SirenOn](as) != 1 {
				t.Fatal("the siren must sound")
			}
		})
	}
}

func TestSmokeWhileDisarmedReturnsToDisarmed(t *testing.T) {
	h := newHarness(t)
	h.signal(smoke, SensorSmoke, true)
	for i := 0; i < 5; i++ {
		h.advance(7 * time.Minute)
	}
	h.mustBe(ModeDisarmed)
	// And an opening door does nothing: the system is not armed.
	noActions(t, h.open(door))
}

func TestSmokeDuringIntrusionAlertsToo(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	as := h.signal(smoke, SensorSmoke, true)
	al := alerts(as)
	if len(al) != 1 || al[0].Kind != AlertFire {
		t.Fatalf("a fire during an intrusion must alert: %v", as)
	}
	if count[SirenOn](as) != 0 {
		t.Fatal("the siren is already ringing")
	}
}

func TestSecondIntrusionSignalDuringAlarmDoesNothing(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	noActions(t, h.moves())
}

func TestPanicIsSilentByDefault(t *testing.T) {
	for _, mode := range []string{"disarmed", "armed"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			if mode == "armed" {
				h.armed()
			}
			before := h.e.Mode()
			as := h.signal(panic_, SensorPanic, true)
			h.mustBe(before)
			al := alerts(as)
			if len(al) != 1 || al[0].Kind != AlertPanic || al[0].Urgency != UrgencyCritical {
				t.Fatalf("panic alert: %+v", al)
			}
			if count[SirenOn](as) != 0 {
				t.Fatal("a silent panic does not sound the siren")
			}
		})
	}
}

func TestPanicWithSirenOption(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.PanicSiren = true })
	as := h.signal(panic_, SensorPanic, true)
	h.mustBe(ModeTriggered)
	if count[SirenOn](as) != 1 || alerts(as)[0].Kind != AlertPanic {
		t.Fatalf("panic with siren: %v", as)
	}
}

func TestPanicDuringIntrusionAlerts(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	as := h.signal(panic_, SensorPanic, true)
	if al := alerts(as); len(al) != 1 || al[0].Kind != AlertPanic {
		t.Fatalf("the panic must alert: %v", as)
	}
}

func TestTamperDisarmedAlertsWithoutSiren(t *testing.T) {
	h := newHarness(t)
	as := h.signal(tamper, SensorTamper, true)
	h.mustBe(ModeDisarmed)
	if al := alerts(as); len(al) != 1 || al[0].Kind != AlertTamper || count[SirenOn](as) != 0 {
		t.Fatalf("tamper while disarmed: %v", as)
	}
}

func TestTamperArmedTriggers(t *testing.T) {
	h := newHarness(t)
	h.armed()
	as := h.signal(tamper, SensorTamper, true)
	h.mustBe(ModeTriggered)
	if al := alerts(as); len(al) != 1 || al[0].Kind != AlertTamper || count[SirenOn](as) != 1 {
		t.Fatalf("tamper while armed: %v", as)
	}
}

func TestPermanentZonesCannotBeBypassed(t *testing.T) {
	h := newHarness(t)
	h.cmd(CmdBypass, func(c *Command) { c.Devices = []string{smoke, tamper} })
	h.signal(smoke, SensorSmoke, true)
	h.mustBe(ModeTriggered)
}
