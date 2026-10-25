package engine

import (
	"slices"
	"testing"
	"time"
)

// --- alarms raised while another one rings ---------------------------------

// fireDuringIntrusion rings an intrusion, then raises a fire 2 minutes into it, and
// returns both incidents.
func fireDuringIntrusion(t *testing.T, h *harness) (intrusion, fire string) {
	t.Helper()
	h.armed()
	intrusion = onlyEvent[Triggered](t, h.breakWindow()).Incident
	h.advance(2 * time.Minute)
	al := alerts(h.signal(smoke, SensorSmoke, true))
	if len(al) != 1 || al[0].Kind != AlertFire {
		t.Fatalf("the fire alerts under its own incident: %v", al)
	}
	return intrusion, al[0].Incident
}

// mustRingFire checks that the fire took the siren, under the incident its alert
// was raised with, and alerted no second time.
func mustRingFire(t *testing.T, h *harness, as []Action, fire string) {
	t.Helper()
	if tr := eventsOf[Triggered](as); len(tr) != 1 || tr[0].Incident != fire || len(alerts(as)) != 0 {
		t.Fatalf("the fire must ring in turn, under its incident: %v", as)
	}
	on := slices.IndexFunc(as, isSirenOn)
	if on < 0 || slices.IndexFunc(as[on:], func(a Action) bool { _, off := a.(SirenOff); return off }) >= 0 {
		t.Fatalf("the siren must be left on: %v", as)
	}
	h.mustBe(ModeTriggered)
	if s := h.e.State(h.now); s.Incident != fire || !s.SirenOn || s.Cycle != 1 {
		t.Fatalf("the state follows the fire: %+v", s)
	}
}

// A fire raised during an intrusion has no siren program of its own while the
// intrusion rings, but the siren must not go quiet for good once the intrusion ends,
// whatever ends it: a code, a disarm, or its cycles running out.
func TestAFireDuringAnIntrusionRingsOnceTheIntrusionEnds(t *testing.T) {
	for _, end := range []struct {
		name string
		do   func(h *harness) []Action
	}{
		{"disarm", func(h *harness) []Action { return h.cmd(CmdDisarm, goodCode("u_1")) }},
		{"silence", func(h *harness) []Action { return h.cmd(CmdSilence, goodCode("u_1")) }},
		{"cycles", func(h *harness) []Action { return h.advance(17 * time.Minute) }}, // 19 min after the intrusion
	} {
		t.Run(end.name, func(t *testing.T) {
			h := newHarness(t)
			_, fire := fireDuringIntrusion(t, h)
			mustRingFire(t, h, end.do(h), fire)
			// Its own cycles, then back to the mode under it.
			h.advance(19 * time.Minute)
			if h.e.Mode() == ModeTriggered {
				t.Fatal("the fire's cycles must end too")
			}
		})
	}
}

func TestASmokeThatClearedBeforeTheIntrusionEndedDoesNotRing(t *testing.T) {
	h := newHarness(t)
	fireDuringIntrusion(t, h)
	h.signal(smoke, SensorSmoke, false)
	as := h.cmd(CmdDisarm, goodCode("u_1"))
	h.mustBe(ModeDisarmed)
	if count[SirenOn](as) != 0 || len(eventsOf[Triggered](as)) != 0 {
		t.Fatalf("nothing left to ring: %v", as)
	}
}

// A window broken during a fire while the site is armed is not lost: it alerts once,
// under its own incident, and the disarm that ends the arming ends its escalation.
func TestAnIntrusionDuringAFireWhileArmedAlertsOnce(t *testing.T) {
	h := newHarness(t)
	h.armed()
	fire := onlyEvent[Triggered](t, h.signal(smoke, SensorSmoke, true)).Incident
	al := alerts(h.breakWindow())
	if len(al) != 1 || al[0].Kind != AlertIntrusion || al[0].Incident == fire {
		t.Fatalf("the intrusion must alert under its own incident: %v", al)
	}
	if len(alerts(h.open(back))) != 0 {
		t.Fatal("one intrusion alert per alarm")
	}
	// Whoever comes in through the entry door, then moves inside, is answering the fire.
	if len(alerts(h.open(door))) != 0 || len(alerts(h.moves())) != 0 {
		t.Fatal("the entry door and the interior give no alert during an alarm")
	}
	h.mustBe(ModeTriggered)
	if s := h.e.State(h.now); s.Incident != fire {
		t.Fatalf("the fire keeps the state: %+v", s)
	}
	as := h.cmd(CmdDisarm, goodCode("u_1"))
	ended := onlyEvent[Disarmed](t, as).Incidents
	if !slices.Contains(ended, al[0].Incident) || !slices.Contains(ended, fire) {
		t.Fatalf("the disarm ends both: %v", ended)
	}

	// Under a fire while disarmed, nothing watches the doors.
	h = newHarness(t)
	h.signal(smoke, SensorSmoke, true)
	if len(alerts(h.breakWindow())) != 0 {
		t.Fatal("an intrusion zone while disarmed")
	}
}

// A tamper during an alarm is reported, and only what was reported is marked so: a
// restart, or the same tamper repeated, does not report it again.
func TestATamperDuringAnAlarmIsReported(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	al := alerts(h.signal(tamper, SensorTamper, true))
	if len(al) != 1 || al[0].Kind != AlertTamper {
		t.Fatalf("the tamper must alert: %v", al)
	}
	if len(alerts(h.signal(tamper, SensorTamper, true))) != 0 {
		t.Fatal("a tamper repeated before it ended is not news")
	}
	// A second device during a tamper alarm is reported too.
	h = newHarness(t)
	h.armed()
	h.signal(tamper, SensorTamper, true)
	if al := alerts(h.signal(smoke, SensorTamper, true)); len(al) != 1 || al[0].Cause.DeviceID != smoke {
		t.Fatalf("another device's tamper: %v", al)
	}
	if ended := onlyEvent[Disarmed](t, h.cmd(CmdDisarm, goodCode("u_1"))).Incidents; len(ended) != 2 {
		t.Fatalf("the disarm ends the tamper alarm and the other tamper: %v", ended)
	}
}

func TestPermanentZonesCannotBeBypassed(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.PanicSiren = true })
	h.cmd(CmdBypass, func(c *Command) { c.Devices = []string{smoke, tamper, panic_} })
	if al := alerts(h.signal(tamper, SensorTamper, true)); len(al) != 1 || al[0].Kind != AlertTamper {
		t.Fatalf("a bypassed tamper zone still reports: %v", al)
	}
	h.signal(smoke, SensorSmoke, true)
	h.mustBe(ModeTriggered)

	h = newHarness(t, func(c *Config) { c.PanicSiren = true })
	h.cmd(CmdBypass, func(c *Command) { c.Devices = []string{panic_} })
	h.signal(panic_, SensorPanic, true)
	h.mustBe(ModeTriggered)
}
