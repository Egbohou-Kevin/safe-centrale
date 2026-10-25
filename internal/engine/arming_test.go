package engine

import (
	"reflect"
	"testing"
	"time"
)

// --- arming ------------------------------------------------------------------

func TestArmThenExitDelayElapses(t *testing.T) {
	h := newHarness(t)
	as := h.arm(func(c *Command) { c.User = "u_42" })

	h.mustBe(ModeArming)
	if r := replyOf(t, as); r.Status != StatusDone {
		t.Fatalf("reply %+v", r)
	}
	ev := onlyEvent[ArmingStarted](t, as)
	if want := t0.Add(2 * time.Minute); !ev.EndsAt.Equal(want) {
		t.Fatalf("EndsAt %v, want %v", ev.EndsAt, want)
	}
	if ev.Origin != OriginKeypad || ev.User != "u_42" || ev.Forced || len(ev.Bypassed) != 0 {
		t.Fatalf("arming event: %+v", ev)
	}
	if got := h.e.NextWake(); !got.Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("NextWake %v", got)
	}

	noActions(t, h.advance(119*time.Second)) // not yet
	h.mustBe(ModeArming)

	as = h.advance(time.Second)
	h.mustBe(ModeArmed)
	armed := onlyEvent[Armed](t, as)
	if armed.Origin != OriginKeypad || armed.User != "u_42" {
		t.Fatalf("armed event: %+v", armed)
	}
	if !h.e.NextWake().IsZero() {
		t.Fatal("armed: no deadline expected")
	}
}

func TestArmNowSkipsExitDelay(t *testing.T) {
	h := newHarness(t)
	h.arm()
	h.now = h.now.Add(10 * time.Second)
	as := h.cmd(CmdArmNow, func(c *Command) { c.Origin = OriginApp })
	h.mustBe(ModeArmed)
	if got := onlyEvent[Armed](t, as); got.Origin != OriginApp {
		t.Fatalf("armed event: %+v", got)
	}
	if !h.e.NextWake().IsZero() {
		t.Fatal("arm_now must clear the deadline")
	}
	// A tick at the old deadline emits nothing more.
	noActions(t, h.advance(2*time.Minute))
}

func TestArmNowRefusedWhenDisarmed(t *testing.T) {
	h := newHarness(t)
	r := replyOf(t, h.cmd(CmdArmNow))
	if r.Status != StatusRefused || r.Reason != ReasonWrongState {
		t.Fatalf("reply %+v", r)
	}
}

func TestDisarmDuringExitDelayCancelsArming(t *testing.T) {
	h := newHarness(t)
	h.arm()
	h.advance(30 * time.Second)
	as := h.cmd(CmdDisarm, goodCode("u_42"))
	h.mustBe(ModeDisarmed)
	if got := onlyEvent[Disarmed](t, as); got.User != "u_42" {
		t.Fatalf("disarmed by %q", got.User)
	}
	noActions(t, h.advance(5*time.Minute)) // nothing else happens
}

func TestArmTwiceIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.arm()
	as := h.arm()
	if r := replyOf(t, as); r.Status != StatusDone || len(eventsOf[ArmingStarted](as)) != 0 {
		t.Fatalf("a second arm must reply done with no new event: %v", as)
	}
}

func TestEntryZonesIgnoredDuringExitDelay(t *testing.T) {
	h := newHarness(t)
	h.arm()
	noActions(t, h.open(door)) // leaving: the door opens
	noActions(t, h.close(door))
	noActions(t, h.moves())
	h.mustBe(ModeArming)
	h.advance(2 * time.Minute)
	h.mustBe(ModeArmed)
}

func TestArmRefusedWhenEntryOrPerimeterOpen(t *testing.T) {
	h := newHarness(t)
	h.open(back)
	h.open(door)
	as := h.arm()
	h.mustBe(ModeDisarmed)
	r := replyOf(t, as)
	if r.Status != StatusRefused || r.Reason != ReasonZoneOpen {
		t.Fatalf("reply %+v", r)
	}
	if got := r.Detail["devices"]; !reflect.DeepEqual(got, []string{back, door}) {
		t.Fatalf("open zones %v (want the sorted list)", got)
	}
	if len(eventsOf[ArmingStarted](as)) != 0 {
		t.Fatal("no arming event on a refusal")
	}
}

func TestOpenInteriorDoesNotBlockArming(t *testing.T) {
	h := newHarness(t)
	h.moves() // motion still active must not block arming
	h.arm()
	h.mustBe(ModeArming)
}

func TestForcedArmBypassesOpenZones(t *testing.T) {
	h := newHarness(t)
	h.open(back)
	as := h.arm(force)
	h.mustBe(ModeArming)
	ev := onlyEvent[ArmingStarted](t, as)
	if !ev.Forced || !reflect.DeepEqual(ev.Bypassed, []string{back}) {
		t.Fatalf("forced arming reported wrongly: %+v", ev)
	}
	h.advance(2 * time.Minute)

	// The bypassed zone has no effect until disarming.
	h.close(back)
	noActions(t, h.open(back))
	h.mustBe(ModeArmed)

	// The other zones still watch.
	h.breakWindow()
	h.mustBe(ModeTriggered)
}

func TestBypassClearedOnDisarm(t *testing.T) {
	h := newHarness(t)
	h.open(back)
	h.arm(force)
	h.advance(2 * time.Minute)
	h.cmd(CmdDisarm, goodCode("u_42"))
	h.close(back)

	h.armed()
	h.open(back) // no longer bypassed
	h.mustBe(ModeTriggered)
}

func TestBypassCommand(t *testing.T) {
	h := newHarness(t)
	h.open(back)
	h.cmd(CmdBypass, func(c *Command) { c.Devices = []string{back} })
	h.arm() // no need to force: the zone is already bypassed
	h.mustBe(ModeArming)

	h.advance(2 * time.Minute)
	r := replyOf(t, h.cmd(CmdBypass, func(c *Command) { c.Devices = []string{window} }))
	if r.Status != StatusRefused || r.Reason != ReasonWrongState {
		t.Fatalf("bypassing while armed must be refused: %+v", r)
	}
}

// An alarm during the exit delay goes back to the exit delay, not to armed, and gives
// it back whole: the person who silences a smoke false alarm at the keypad is still
// inside, and arming at once would set off an intrusion with their next step.
func TestAnAlarmDuringTheExitDelayGivesTheExitDelayBack(t *testing.T) {
	h := newHarness(t)
	h.arm(func(c *Command) { c.User = "u_42" })
	h.advance(20 * time.Second)
	h.signal(smoke, SensorSmoke, true)
	h.signal(smoke, SensorSmoke, false)
	h.advance(time.Minute)
	as := h.cmd(CmdSilence, goodCode("u_42"))
	h.mustBe(ModeArming)
	ev := onlyEvent[ArmingStarted](t, as)
	if !ev.EndsAt.Equal(h.now.Add(h.cfg.ExitDelay)) || ev.User != "u_42" {
		t.Fatalf("the exit delay starts again, whole: %+v", ev)
	}
	if as := h.moves(); len(eventsOf[Triggered](as)) != 0 {
		t.Fatalf("whoever silenced the siren walks out: %v", as)
	}
	if armed := onlyEvent[Armed](t, h.advance(h.cfg.ExitDelay)); armed.User != "u_42" {
		t.Fatalf("armed at the end of the delay: %+v", armed)
	}
}

// Nobody there: the siren runs out, the exit delay starts again, and its end does what
// every end of an exit delay does: alarm.armed, and a window left open is bypassed and
// told.
func TestAnAlarmThatRunsOutDuringTheExitDelayEndsTheArming(t *testing.T) {
	h := newHarness(t)
	h.arm()
	h.advance(20 * time.Second)
	h.open(window)
	h.signal(smoke, SensorSmoke, true)
	h.signal(smoke, SensorSmoke, false)
	h.advance(19 * time.Minute)
	h.mustBe(ModeArming)
	as := h.advance(h.cfg.ExitDelay)
	h.mustBe(ModeArmed)
	onlyEvent[Armed](t, as)
	if al := alerts(as); len(al) != 1 || al[0].Kind != AlertArmingBypassed || !reflect.DeepEqual(al[0].Devices, []string{window}) {
		t.Fatalf("the open window is bypassed and told: %v", al)
	}
}
