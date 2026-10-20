package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
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
	if as[len(as)-1] != (SirenOn{Duration: 4 * time.Minute}) {
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
	if !want.Bypassed[back] || !want.Open[back] || want.Failed != 1 {
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
