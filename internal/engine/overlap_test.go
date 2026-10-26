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
	h.arm()
	if r := replyOf(t, h.cmd(CmdBypass, func(c *Command) { c.Devices = []string{smoke, tamper, panic_} })); r.Status != StatusRefused {
		t.Fatalf("a permanent zone is never bypassed: %+v", r)
	}
	if al := alerts(h.signal(tamper, SensorTamper, true)); len(al) != 1 || al[0].Kind != AlertTamper {
		t.Fatalf("a bypassed tamper zone still reports: %v", al)
	}
	h.signal(smoke, SensorSmoke, true)
	h.mustBe(ModeTriggered)

	h = newHarness(t, func(c *Config) { c.PanicSiren = true })
	h.arm()
	h.cmd(CmdBypass, func(c *Command) { c.Devices = []string{panic_} })
	h.signal(panic_, SensorPanic, true)
	h.mustBe(ModeTriggered)
}

// smoke2 is a second fire detector, in the stock room.
const smoke2 = "dev_smoke2"

func withSmoke2(c *Config) { c.Zones[smoke2] = Zone{Name: "stock", Kind: ZoneFire} }

// A fire set off again during an intrusion, after its first detector went back to
// rest, is not lost: it alerts again (the first alert may have been answered "only
// steam") and the siren rings for it once the intrusion ends. The same goes for a
// second detector, and for one that stays in alarm while the first one clears.
func TestAFireSetOffAgainDuringAnIntrusionStillRings(t *testing.T) {
	for _, c := range []struct {
		name  string
		again func(h *harness) []Action
	}{
		{"same detector", func(h *harness) []Action { return h.signal(smoke, SensorSmoke, true) }},
		{"other detector", func(h *harness) []Action { return h.signal(smoke2, SensorSmoke, true) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, withSmoke2)
			_, first := fireDuringIntrusion(t, h)
			h.signal(smoke, SensorSmoke, false)
			al := alerts(c.again(h))
			if len(al) != 1 || al[0].Kind != AlertFire || al[0].Incident == first {
				t.Fatalf("a fire after the first one cleared alerts under a new incident: %v", al)
			}
			mustRingFire(t, h, h.cmd(CmdDisarm, goodCode("u_1")), al[0].Incident)
		})
	}

	// A second detector while the first one waits joins its incident; the first one
	// clearing does not take the siren from the second.
	h := newHarness(t, withSmoke2)
	_, fire := fireDuringIntrusion(t, h)
	if al := alerts(h.signal(smoke2, SensorSmoke, true)); len(al) != 0 {
		t.Fatalf("one fire alert while a fire waits: %v", al)
	}
	h.signal(smoke, SensorSmoke, false)
	mustRingFire(t, h, h.cmd(CmdDisarm, goodCode("u_1")), fire)
	h.advance(19 * time.Minute)
	h.mustBe(ModeDisarmed) // the fire rang once, not once per detector
}

// A tamper (or a fire) during an entry delay rings at once, but does not take the
// intrusion's place: nobody typed a code before the delay ran out, so whoever came in
// is an intruder, with an intrusion's alert and escalation, not a tamper's single
// message.
func TestAnAlarmDuringAnEntryDelayDoesNotDropTheIntrusion(t *testing.T) {
	for _, other := range []struct {
		name string
		kind AlertKind
		do   func(h *harness) []Action
	}{
		{"tamper", AlertTamper, func(h *harness) []Action { return h.signal(tamper, SensorTamper, true) }},
		{"fire", AlertFire, func(h *harness) []Action { return h.signal(smoke, SensorSmoke, true) }},
	} {
		t.Run(other.name, func(t *testing.T) {
			h := newHarness(t)
			h.armed()
			h.open(door)
			h.advance(10 * time.Second)
			first := onlyEvent[Triggered](t, other.do(h)).Incident
			h.e = restart(t, h) // what is left of the entry delay is saved
			h.e.Boot(h.now, nil)
			if len(alerts(h.moves())) != 0 {
				t.Fatal("motion during the entry delay is the person coming in")
			}
			al := alerts(h.advance(h.cfg.EntryDelay - 10*time.Second))
			if len(al) != 1 || al[0].Kind != AlertIntrusion || al[0].Incident == first {
				t.Fatalf("the entry delay that ran out alerts an intrusion under its own incident: %v", al)
			}
			h.mustBe(ModeTriggered)
			if s := h.e.State(h.now); s.Incident != first {
				t.Fatalf("the %s keeps the siren: %+v", other.name, s)
			}
			ended := onlyEvent[Disarmed](t, h.cmd(CmdDisarm, goodCode("u_1"))).Incidents
			if !slices.Contains(ended, al[0].Incident) {
				t.Fatalf("the disarm ends the intrusion's escalation: %v", ended)
			}
		})
	}

	// Whoever disarms in time is no intruder.
	h := newHarness(t)
	h.armed()
	h.open(door)
	h.signal(tamper, SensorTamper, true)
	h.advance(20 * time.Second)
	h.cmd(CmdDisarm, goodCode("u_1"))
	if al := alerts(h.advance(time.Hour)); len(al) != 0 {
		t.Fatalf("no intrusion after a disarm: %v", al)
	}
}

// An alarm silenced before the entry delay it interrupted ran out gives the rest of
// the delay back: the code that silenced the siren did not disarm.
func TestTheEntryDelayGoesOnOnceTheAlarmThatInterruptedItIsSilenced(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.open(door)
	h.advance(10 * time.Second)
	h.signal(tamper, SensorTamper, true)
	h.advance(10 * time.Second)
	h.cmd(CmdSilence, goodCode("u_1"))
	h.mustBe(ModeEntryDelay)
	if s := h.e.State(h.now); !s.EndsAt.Equal(t0.Add(h.cfg.ExitDelay + h.cfg.EntryDelay)) {
		t.Fatalf("the entry delay keeps its end: %+v", s)
	}
	as := h.advance(h.cfg.EntryDelay - 20*time.Second)
	if tr := onlyEvent[Triggered](t, as); tr.Cause.DeviceID != door || count[SirenOn](as) != 1 {
		t.Fatalf("the entry delay that ran out rings the intrusion: %v", as)
	}
}

// A fire during a fire, set off once every detector of the one that rings went back
// to rest, is a new fire: it alerts under its own incident (the first alert may have
// been answered "only steam in the kitchen") and rings once the first one ends,
// whatever ends it. While a detector of the ringing fire is still in alarm, another
// one joins it, without a second alert.
func TestAFireAfterTheRingingFireClearedIsANewFire(t *testing.T) {
	for _, end := range []struct {
		name string
		do   func(h *harness) []Action
	}{
		{"silence", func(h *harness) []Action { return h.cmd(CmdSilence, goodCode("u_1")) }},
		{"cycles", func(h *harness) []Action { return h.advance(18 * time.Minute) }}, // 19 min after the first
	} {
		t.Run(end.name, func(t *testing.T) {
			h := newHarness(t, withSmoke2)
			first := onlyEvent[Triggered](t, h.signal(smoke, SensorSmoke, true)).Incident
			h.signal(smoke, SensorSmoke, false)
			h.advance(time.Minute)
			al := alerts(h.signal(smoke2, SensorSmoke, true))
			if len(al) != 1 || al[0].Kind != AlertFire || al[0].Incident == first || al[0].Cause.DeviceID != smoke2 {
				t.Fatalf("a fire after the ringing one cleared alerts under a new incident: %v", al)
			}
			mustRingFire(t, h, end.do(h), al[0].Incident)
			h.advance(19 * time.Minute)
			h.mustBe(ModeDisarmed)
		})
	}

	// While the first detector is still in smoke, the second joins its fire.
	h := newHarness(t, withSmoke2)
	h.signal(smoke, SensorSmoke, true)
	if al := alerts(h.signal(smoke2, SensorSmoke, true)); len(al) != 0 {
		t.Fatalf("one fire alert while the fire rings: %v", al)
	}
	// The first one clearing leaves the second in alarm: a third joins too.
	h.signal(smoke, SensorSmoke, false)
	if al := alerts(h.signal(smoke, SensorSmoke, true)); len(al) != 0 {
		t.Fatalf("the fire still rings for the second detector: %v", al)
	}
}
