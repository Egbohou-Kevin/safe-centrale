package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"
)

// --- persistence and restart ------------------------------------------------

func restart(t *testing.T, h *harness) *Engine {
	t.Helper()
	b, err := json.Marshal(h.e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	e, err := New(h.cfg, func() string { h.ids++; return fmt.Sprintf("inc-%d", h.ids) }, h.now)
	if err != nil {
		t.Fatal(err)
	}
	e.Restore(snap)
	return e
}

func TestRestartWhileArmedStaysArmed(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.now = h.now.Add(time.Minute)
	h.e = restart(t, h)
	noActions(t, h.e.Boot(h.now, nil))
	h.mustBe(ModeArmed)
	h.open(back)
	h.mustBe(ModeTriggered)
}

func TestRestartWhileArmingKeepsRemainingDelay(t *testing.T) {
	h := newHarness(t)
	h.arm()
	h.now = h.now.Add(30 * time.Second)
	h.e = restart(t, h)
	noActions(t, h.e.Boot(h.now, nil))
	h.mustBe(ModeArming)
	h.advance(90 * time.Second) // 2 min since arming
	h.mustBe(ModeArmed)
}

func TestRestartAfterDelayElapsedFinishesArming(t *testing.T) {
	h := newHarness(t)
	h.arm()
	restarted := restart(t, h)
	h.now = h.now.Add(10 * time.Minute) // the hub stayed off
	h.e = restarted
	as := h.e.Boot(h.now, nil)
	h.mustBe(ModeArmed)
	onlyEvent[Armed](t, as)
}

func TestRestartWhileTriggeredResumesSiren(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	h.now = h.now.Add(time.Minute)
	h.e = restart(t, h)

	as := h.e.Boot(h.now, nil)
	h.mustBe(ModeTriggered)
	var on []SirenOn
	for _, a := range as {
		if s, ok := a.(SirenOn); ok {
			on = append(on, s)
		}
	}
	if len(on) != 1 || on[0].Duration != 4*time.Minute {
		t.Fatalf("the siren must resume for the 4 remaining minutes: %v", as)
	}
	// The schedule goes on: the end of cycle 1 is 5 minutes after the alarm.
	if count[SirenOff](h.advance(4*time.Minute)) != 1 {
		t.Fatal("end of cycle expected")
	}
}

func TestRestartAcrossACycleStartResumesTheSirenOnce(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	// The hub was off from the alarm until 8 minutes later: cycle 1 ended at 5,
	// cycle 2 started at 7 and ends at 12.
	h.e = restart(t, h)
	h.now = h.now.Add(8 * time.Minute)
	as := h.e.Boot(h.now, nil)
	var on []SirenOn
	for _, a := range as {
		if s, ok := a.(SirenOn); ok {
			on = append(on, s)
		}
	}
	if len(on) != 1 || on[0].Duration != 4*time.Minute {
		t.Fatalf("the siren must sound once, for the 4 minutes left of cycle 2: %v", as)
	}
	off := slices.IndexFunc(as, func(a Action) bool { _, ok := a.(SirenOff); return ok })
	at := slices.IndexFunc(as, func(a Action) bool { _, ok := a.(SirenOn); return ok })
	if off < 0 || at < off {
		t.Fatalf("the siren order must come after the replayed end of cycle 1: %v", as)
	}
	onlyEvent[SirenCycleStarted](t, as)
}

func TestRestartDuringSirenPauseDoesNotResumeSiren(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	h.advance(6 * time.Minute) // pause between cycles 1 and 2
	h.e = restart(t, h)
	if count[SirenOn](h.e.Boot(h.now, nil)) != 0 {
		t.Fatal("no siren during the pause")
	}
}

// The state published after a restart in the middle of an alarm keeps to the siren's
// schedule, which the snapshot holds: the end of the cycle or of the pause under way
// stays where it was, and the steps missed while the hub was off are counted at
// their own time, not from the restart.
func TestRestartKeepsTheSirenScheduleInTheState(t *testing.T) {
	for _, c := range []struct {
		name        string
		saved, back time.Duration // since the alarm: the last snapshot, and the restart
		want        sirenAt
	}{
		{"in a cycle", 2 * time.Minute, 3 * time.Minute, sirenAt{1, true, 5 * time.Minute}},
		{"in the pause", 6 * time.Minute, 6*time.Minute + 30*time.Second, sirenAt{1, false, 7 * time.Minute}},
		{"off at the end of a cycle", 4 * time.Minute, 6 * time.Minute, sirenAt{1, false, 7 * time.Minute}},
		{"off across a cycle start", time.Minute, 8 * time.Minute, sirenAt{2, true, 12 * time.Minute}},
		{"off into the last cycle", 6 * time.Minute, 16 * time.Minute, sirenAt{3, true, 19 * time.Minute}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.armed()
			start := h.now
			inc := onlyEvent[Triggered](t, h.breakWindow()).Incident
			h.now = start.Add(c.saved)
			h.e.Handle(Tick{Now: h.now})
			h.e = restart(t, h)
			h.now = start.Add(c.back)
			h.e.Boot(h.now, nil)
			h.mustRing(inc, start, c.want)
		})
	}

	// The cycles ran out while the hub was off: armed again, nothing left to say.
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	h.e = restart(t, h)
	h.now = h.now.Add(20 * time.Minute)
	h.e.Boot(h.now, nil)
	if s := h.e.State(h.now); s.Mode != ModeArmed || s.Incident != "" || s.SirenOn || !s.EndsAt.IsZero() {
		t.Fatalf("after the cycles ran out: %+v", s)
	}
}

func TestBootTriggersOnZonesAlreadyInAlarm(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.e = restart(t, h)
	as := h.e.Boot(h.now, []Signal{
		{DeviceID: back, Sensor: SensorContact, Active: true},
		{DeviceID: door, Sensor: SensorContact, Active: false},
	})
	h.mustBe(ModeTriggered)
	if count[SirenOn](as) != 1 || len(alerts(as)) != 1 {
		t.Fatalf("a back door open at restart must trigger: %v", as)
	}
}

func TestBootDisarmedLearnsOpenDoors(t *testing.T) {
	h := newHarness(t)
	h.e = restart(t, h)
	noActions(t, h.e.Boot(h.now, []Signal{{DeviceID: back, Sensor: SensorContact, Active: true}}))
	r := replyOf(t, h.arm())
	if r.Reason != ReasonZoneOpen {
		t.Fatalf("a door known open at boot must block arming: %+v", r)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	h := newHarness(t)
	h.open(back)
	snap := h.e.Snapshot()
	snap.Open["dev_ghost"] = true
	snap.Mode = ModeTriggered
	if h.e.Mode() != ModeDisarmed || h.e.Snapshot().Open["dev_ghost"] {
		t.Fatal("changing a copy must not change the engine")
	}
}

func TestSnapshotJSONRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.open(back)
	h.arm(force) // the back door is bypassed: the snapshot keeps that detail
	h.advance(h.cfg.ExitDelay)
	h.mustBe(ModeArmed)
	h.cmd(CmdDisarm, badCode)
	want := h.e.Snapshot()
	if !want.Bypassed[back] || !want.Open[back] || len(want.Failures) != 1 {
		t.Fatalf("the snapshot must hold what we want to save: %+v", want)
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Snapshot
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON round trip:\n got %+v\nwant %+v", got, want)
	}
}

// A door opened during the exit delay and closed while the hub was off: the exit
// delay replayed at Boot must see it closed, or it would start the entry delay
// (entry door) or bypass the zone all night (perimeter).
func TestRestartAfterTheExitDelayUsesTheDoorsAsTheyAreNow(t *testing.T) {
	for _, dev := range []string{door, back} {
		t.Run(dev, func(t *testing.T) {
			h := newHarness(t)
			h.arm()
			h.open(dev) // during the exit delay: nothing happens yet
			h.e = restart(t, h)
			h.now = h.now.Add(10 * time.Minute)
			as := h.e.Boot(h.now, []Signal{{DeviceID: dev, Sensor: SensorContact, Active: false}})
			h.mustBe(ModeArmed)
			onlyEvent[Armed](t, as)
			if len(alerts(as)) != 0 || h.e.Snapshot().Bypassed[dev] || h.e.Snapshot().Open[dev] {
				t.Fatalf("a closed door must leave the zone armed: %v %+v", as, h.e.Snapshot())
			}
			h.open(dev)
			if m := h.e.Mode(); m != ModeEntryDelay && m != ModeTriggered {
				t.Fatalf("the zone must still be watched, mode %q", m)
			}
		})
	}
}

// The same doors still open at Boot are dealt with as at the end of any exit delay.
func TestRestartAfterTheExitDelayWithDoorsStillOpen(t *testing.T) {
	h := newHarness(t)
	h.arm()
	h.open(back)
	h.e = restart(t, h)
	h.now = h.now.Add(10 * time.Minute)
	as := h.e.Boot(h.now, []Signal{{DeviceID: back, Sensor: SensorContact, Active: true}})
	h.mustBe(ModeArmed)
	if al := alerts(as); len(al) != 1 || al[0].Kind != AlertArmingBypassed || !h.e.Snapshot().Bypassed[back] {
		t.Fatalf("a window still open is bypassed and reported once: %v", as)
	}

	h = newHarness(t)
	h.arm()
	h.open(door)
	h.e = restart(t, h)
	h.now = h.now.Add(10 * time.Minute)
	as = h.e.Boot(h.now, []Signal{{DeviceID: door, Sensor: SensorContact, Active: true}})
	h.mustBe(ModeTriggered)
	if al := alerts(as); len(al) != 1 || al[0].Kind != AlertIntrusion || count[SirenOn](as) != 1 {
		t.Fatalf("an entry door open since the exit delay ends in one alarm: %v", as)
	}
}

// Between Restore and Boot (the boot window) the engine replays no deadline: an input
// in that window is applied to the saved state, and the deadlines wait for Boot.
func TestNoDeadlineIsReplayedBeforeBoot(t *testing.T) {
	h := newHarness(t)
	h.arm()
	h.open(door)
	h.e = restart(t, h)
	h.now = h.now.Add(10 * time.Minute)
	noActions(t, h.close(door)) // arrives during the boot window
	h.mustBe(ModeArming)
	as := h.e.Boot(h.now, nil)
	h.mustBe(ModeArmed)
	onlyEvent[Armed](t, as)
}

// A tamper reported before the restart and still active is not reported again: a
// restart while armed must not sound the siren for it. One that appeared while the
// hub was off is new.
func TestBootReportsOnlyNewTampers(t *testing.T) {
	const keypad = "dev_keypad" // in no zone
	h := newHarness(t)
	h.signal(keypad, SensorTamper, true) // reported while disarmed
	h.armed()
	h.e = restart(t, h)
	tampered := []Signal{
		{DeviceID: keypad, Sensor: SensorTamper, Active: true},
		{DeviceID: smoke, Sensor: SensorTamper, Active: true}, // new
	}
	as := h.e.Boot(h.now, tampered)
	al := alerts(as)
	if len(al) != 1 || al[0].Kind != AlertTamper || al[0].Cause.DeviceID != smoke {
		t.Fatalf("only the new tamper is reported: %v", as)
	}
	h.cmd(CmdDisarm, goodCode("u_1"))

	// Both are now known: a second restart reports nothing.
	h.e = restart(t, h)
	noActions(t, h.e.Boot(h.now, tampered))

	// A tamper that ended while the hub was off is forgotten: the next one is new.
	h.e = restart(t, h)
	h.e.Boot(h.now, []Signal{{DeviceID: keypad, Sensor: SensorTamper, Active: false}})
	h.e = restart(t, h)
	if al := alerts(h.e.Boot(h.now, tampered[:1])); len(al) != 1 {
		t.Fatalf("a tamper back after it ended is new: %v", al)
	}
}
