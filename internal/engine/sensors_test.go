package engine

import (
	"slices"
	"testing"
	"time"
)

// --- which sensor counts for which zone ---------------------------------------

// A device's tamper sensor means tamper whatever its zone: opening the cover of a
// smoke detector is not a fire, the keypad's tamper is not a panic, and a door
// contact's tamper does not start the entry delay.
func TestATamperIsATamperWhateverTheZone(t *testing.T) {
	for _, dev := range []string{door, back, window, motion, smoke, panic_, tamper} {
		t.Run(dev, func(t *testing.T) {
			h := newHarness(t)
			as := h.signal(dev, SensorTamper, true)
			h.mustBe(ModeDisarmed)
			if al := alerts(as); len(al) != 1 || al[0].Kind != AlertTamper || count[SirenOn](as) != 0 {
				t.Fatalf("disarmed: want one tamper alert without siren, got %v", as)
			}
			if al := alerts(as)[0]; al.Cause == nil || al.Cause.DeviceID != dev || al.Cause.Sensor != SensorTamper {
				t.Fatalf("cause %+v", al.Cause)
			}

			h = newHarness(t)
			h.armed()
			as = h.signal(dev, SensorTamper, true)
			h.mustBe(ModeTriggered)
			if al := alerts(as); len(al) != 1 || al[0].Kind != AlertTamper {
				t.Fatalf("armed: want a tamper alarm, got %v", as)
			}
			if len(eventsOf[EntryDelayStarted](as)) != 0 {
				t.Fatal("a tamper never starts the entry delay")
			}
		})
	}
}

// A device that belongs to no zone (a keypad, a siren) still reports its tamper.
func TestAnUnzonedDeviceStillReportsItsTamper(t *testing.T) {
	h := newHarness(t)
	as := h.signal("dev_keypad", SensorTamper, true)
	al := alerts(as)
	if len(al) != 1 || al[0].Kind != AlertTamper || al[0].Cause == nil || al[0].Cause.DeviceID != "dev_keypad" || al[0].Cause.Zone != "" {
		t.Fatalf("want a tamper alert without a zone, got %v", as)
	}
	noActions(t, h.signal("dev_keypad", SensorTamper, false))
	noActions(t, h.signal("dev_keypad", SensorPanic, true)) // only the tamper counts without a zone
}

// A zone reacts only to the sensors its kind accepts.
func TestAZoneIgnoresTheSensorsItsKindDoesNotUse(t *testing.T) {
	h := newHarness(t)
	noActions(t, h.signal(smoke, SensorMotion, true))
	noActions(t, h.signal(panic_, SensorContact, true))
	noActions(t, h.signal(tamper, SensorSmoke, true))
	h.armed()
	noActions(t, h.signal(door, SensorSmoke, true))
	noActions(t, h.signal(motion, SensorPanic, true))
	noActions(t, h.signal(window, SensorSmoke, true))
	h.mustBe(ModeArmed)
}

// A smoke detector's contact (if it had one) never holds a zone open.
func TestOnlyZonesThatWatchContactsTrackThem(t *testing.T) {
	h := newHarness(t)
	h.signal(smoke, SensorContact, true)
	if len(h.e.Snapshot().Open) != 0 {
		t.Fatalf("open: %v", h.e.Snapshot().Open)
	}
}

func TestAcceptsMatchesTheZoneTable(t *testing.T) {
	for _, c := range []struct {
		kind   ZoneKind
		sensor SensorKind
		want   bool
	}{
		{ZoneEntry, SensorContact, true},
		{ZoneInstant, SensorGlassBreak, true},
		{ZoneInstant, SensorVibration, true},
		{ZoneInterior, SensorMotion, true},
		{ZoneFire, SensorSmoke, true},
		{ZonePanic, SensorPanic, true},
		{ZoneTamper, SensorTamper, true},
		{ZoneFire, SensorTamper, false},
		{ZoneFire, SensorMotion, false},
		{ZonePanic, SensorTamper, false},
		{ZoneEntry, SensorSmoke, false},
		{"lobby", SensorContact, false},
	} {
		if got := Accepts(c.kind, c.sensor); got != c.want {
			t.Errorf("Accepts(%s, %s) = %v, want %v", c.kind, c.sensor, got, c.want)
		}
	}
	kinds := ZoneKinds()
	if len(kinds) != 6 || !slices.IsSorted(kinds) {
		t.Fatalf("ZoneKinds: %v", kinds)
	}
	for _, k := range kinds {
		if !k.valid() || len(k.Sensors()) == 0 {
			t.Errorf("%s: valid %v, sensors %v", k, k.valid(), k.Sensors())
		}
	}
	s := ZoneFire.Sensors()
	s[0] = SensorMotion
	if !Accepts(ZoneFire, SensorSmoke) {
		t.Fatal("Sensors must return a copy")
	}
}

// --- forgetting a removed device -------------------------------------------------

// A device removed from the site no longer holds a door open, nor a bypass.
func TestForgetDropsARemovedDevice(t *testing.T) {
	h := newHarness(t)
	h.open(back)
	if r := replyOf(t, h.arm()); r.Reason != ReasonZoneOpen {
		t.Fatalf("an open door blocks arming: %+v", r)
	}
	h.arm(force) // bypasses the back door
	noActions(t, h.e.Handle(Forget{Now: h.now, DeviceID: back}))
	s := h.e.Snapshot()
	if len(s.Open) != 0 || len(s.Bypassed) != 0 {
		t.Fatalf("open %v, bypassed %v", s.Open, s.Bypassed)
	}
	h.cmd(CmdDisarm, goodCode("u_42"))
	h.arm()
	h.mustBe(ModeArming)
	noActions(t, h.e.Handle(Forget{Now: h.now, DeviceID: "dev_never_seen"}))
}

// --- end of the exit delay with a zone still open ----------------------------------

// The door was opened while leaving and is still open when the exit delay ends:
// contacts report changes only, so the entry delay must start now or never.
func TestAnEntryDoorStillOpenWhenTheExitDelayEndsStartsTheEntryDelay(t *testing.T) {
	h := newHarness(t)
	h.arm()
	h.open(door)
	as := h.advance(h.cfg.ExitDelay)
	h.mustBe(ModeEntryDelay)
	onlyEvent[Armed](t, as)
	ev := onlyEvent[EntryDelayStarted](t, as)
	if ev.DeviceID != door || !ev.EndsAt.Equal(h.now.Add(h.cfg.EntryDelay)) {
		t.Fatalf("entry delay %+v", ev)
	}
	as = h.advance(h.cfg.EntryDelay)
	h.mustBe(ModeTriggered)
	if tr := onlyEvent[Triggered](t, as); tr.Cause.DeviceID != door {
		t.Fatalf("cause %+v", tr.Cause)
	}
}

// arm_now ends the exit delay the same way.
func TestArmNowWithTheEntryDoorOpenStartsTheEntryDelay(t *testing.T) {
	h := newHarness(t)
	h.arm()
	h.open(door)
	h.cmd(CmdArmNow)
	h.mustBe(ModeEntryDelay)
}

// A window opened during the exit delay and left open: nobody is at the keypad to be
// told, so the zone is bypassed and the contacts are alerted, as the automatic arming
// does.
func TestAWindowStillOpenWhenTheExitDelayEndsIsBypassedAndReported(t *testing.T) {
	h := newHarness(t)
	h.arm()
	h.open(back)
	as := h.advance(h.cfg.ExitDelay)
	h.mustBe(ModeArmed)
	al := alerts(as)
	if len(al) != 1 || al[0].Kind != AlertArmingBypassed || !slices.Equal(al[0].Devices, []string{back}) {
		t.Fatalf("want an arming_bypassed alert for the back door, got %v", as)
	}
	if count[SirenOn](as) != 0 {
		t.Fatal("no siren for a door left open")
	}
	h.close(back)
	noActions(t, h.open(back)) // bypassed until disarming
	h.breakWindow()
	h.mustBe(ModeTriggered)
}

// A zone bypassed by a forced arming stays quiet at the end of the exit delay.
func TestABypassedOpenZoneDoesNothingWhenTheExitDelayEnds(t *testing.T) {
	h := newHarness(t)
	h.open(door)
	h.open(back)
	h.arm(force)
	as := h.advance(h.cfg.ExitDelay)
	h.mustBe(ModeArmed)
	if len(alerts(as)) != 0 || len(eventsOf[EntryDelayStarted](as)) != 0 {
		t.Fatalf("%v", as)
	}
}

// --- late ticks and restarts ---------------------------------------------------------

// A deadline handled late (the hub was busy or off) sounds only what is left of the
// cycle that started meanwhile, once.
func TestALateTickSoundsOnlyWhatIsLeftOfTheCycle(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	as := h.advance(8 * time.Minute) // cycle 1 ended at 5, cycle 2 started at 7
	var on []SirenOn
	for _, a := range as {
		if s, ok := a.(SirenOn); ok {
			on = append(on, s)
		}
	}
	if len(on) != 1 || on[0].Duration != 4*time.Minute {
		t.Fatalf("want one siren order for the 4 minutes left, got %v", as)
	}
	if count[SirenOn](h.advance(4*time.Minute)) != 0 {
		t.Fatal("cycle 2 ends on time")
	}
}

// A late tick that ends in the pause sounds nothing.
func TestALateTickEndingInThePauseLeavesTheSirenOff(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	as := h.advance(13 * time.Minute) // cycle 2 ran from 7 to 12, pause until 14
	if count[SirenOn](as) != 0 || count[SirenOff](as) == 0 {
		t.Fatalf("%v", as)
	}
}

// After a restore, the engine asks for no tick until Boot: Boot is what resumes the
// siren, once, for what is left of the cycle.
func TestNoWakeBeforeBootAfterARestore(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	h.e = restart(t, h)
	if !h.e.NextWake().IsZero() {
		t.Fatalf("NextWake before Boot: %v", h.e.NextWake())
	}
	h.now = h.now.Add(8 * time.Minute)
	h.e.Boot(h.now, time.Time{}, nil)
	if want := t0.Add(2*time.Minute + 12*time.Minute); !h.e.NextWake().Equal(want) {
		t.Fatalf("NextWake after Boot %v, want %v", h.e.NextWake(), want)
	}
}
