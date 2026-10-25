package video

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// restarted is the same service after a restart, at now.
func (h *harness) restarted(now time.Time) *harness {
	h.t.Helper()
	v, err := New(h.v.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	v.Restore(h.v.Snapshot())
	return &harness{t: h.t, v: v, now: now}
}

// The hub boots with its clock in an earlier month (a dead clock module), then the
// network sets it right: the usage the client paid for this month on mobile data is
// still counted, and the allowance still holds.
func TestAClockSetBackKeepsTheMonthsUsage(t *testing.T) {
	h := newHarness(t, func(c *Config) { mobile(c); c.Quota = 1_000_000 })
	h.request("s1", "cam_caisse", 0)
	h.progress("s1", 900_000)
	h.v.Handle(Stop{Now: h.now, Session: "s1"})
	h = h.restarted(time.Date(2025, 3, 1, 8, 0, 0, 0, time.UTC))
	h.request("s2", "cam_caisse", 0)
	if acts := h.progress("s2", 200_000); !halted(acts, "s2") {
		t.Fatalf("the allowance holds under a clock set back: %+v", acts)
	}
	h.now = t0.Add(time.Hour) // the network sets the clock right
	if r := only(t, h.request("s3", "cam_caisse", 0)); r.Reason != Quota || h.v.State().Used != 1_100_000 {
		t.Fatalf("%+v, used %d", r, h.v.State().Used)
	}
}

// Photos taken while the clock was a month ahead do not keep the alarms of the real
// time from being photographed for a month: an hour at most.
func TestPhotosDatedAheadDoNotBlockForLong(t *testing.T) {
	h := newHarness(t)
	ahead := t0.Add(30 * 24 * time.Hour)
	for i := range 6 {
		h.v.Handle(Alarm{Now: ahead, Incident: fmt.Sprint("inc", i)})
	}
	h = h.restarted(t0)
	h.v.Handle(Alarm{Now: h.now, Incident: "inc6"})
	h.now = t0.Add(time.Hour)
	if acts := h.v.Handle(Alarm{Now: h.now, Incident: "inc7"}); len(acts) != 1 {
		t.Fatalf("%+v", acts)
	} else if _, ok := acts[0].(TakePhotos); !ok {
		t.Fatalf("an hour later: %+v", acts)
	}
}

// A live session does not rewrite the saved state at every progress (every 2 s): the
// usage is saved every 10 MB and when the session ends. What is published is exact.
func TestALiveSessionSavesItsUsageInSteps(t *testing.T) {
	h := newHarness(t)
	h.request("s1", "cam_caisse", 0)
	saved := func() string { b, _ := json.Marshal(h.v.Snapshot()); return string(b) }
	before, changes := saved(), 0
	var sent int64
	for range 60 {
		h.now = h.now.Add(2 * time.Second)
		sent += 250_000
		h.progress("s1", sent)
		if s := saved(); s != before {
			before, changes = s, changes+1
		}
	}
	if changes != 1 || h.v.State().Used != sent {
		t.Fatalf("%d saves for 15 MB, used %d", changes, h.v.State().Used)
	}
	h.v.Handle(Stop{Now: h.now, Session: "s1"})
	if h.v.Snapshot().Used != sent {
		t.Fatalf("saved %d at the end, want %d", h.v.Snapshot().Used, sent)
	}
}

// The hub boots with its clock years ahead (a clock module set wrong), and the usage
// starts again in that month. Once NTP vouches for the clock set right, the month is
// the real one again, with what the client used in it before and since: the allowance
// still holds, and starts again at the real next month, not years later.
func TestAMonthSetAheadIsTakenBackOnceTheClockIsVouchedFor(t *testing.T) {
	h := newHarness(t, func(c *Config) { mobile(c); c.Quota = 1_000_000 })
	h.request("s1", "cam_caisse", 0)
	h.progress("s1", 960_000)
	h.v.Handle(Stop{Now: h.now, Session: "s1"})
	h = h.restarted(t0.AddDate(3, 0, 0))
	h.request("s2", "cam_caisse", 0)
	h.progress("s2", 50_000)
	h.v.Handle(Stop{Now: h.now, Session: "s2"})
	h = h.restarted(t0.Add(time.Hour)) // NTP sets the clock right
	h.v.Handle(Tick{Now: h.now})
	if used := h.v.State().Used; used != 50_000 {
		t.Fatalf("a clock not vouched for moves no month back: used %d", used)
	}
	h.v.Handle(Tick{Now: h.now, Synced: true})
	if r := only(t, h.request("s3", "cam_caisse", 0)); r.Reason != Quota || h.v.State().Used != 1_010_000 {
		t.Fatalf("%+v, used %d", r, h.v.State().Used)
	}
	h.now = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if r := only(t, h.request("s4", "cam_caisse", 0)); r.Status != Starting || h.v.State().Used != 0 {
		t.Fatalf("the real next month: %+v, used %d", r, h.v.State().Used)
	}
}

// The cameras are checked at once after a restart: neither a check time saved while
// the clock ran ahead, nor what they answered before, is waited on.
func TestCamerasAreCheckedAtOnceAfterARestart(t *testing.T) {
	h := newHarness(t)
	h.now = t0.AddDate(1, 0, 0)
	h.v.Handle(Tick{Now: h.now})
	h = h.restarted(t0)
	if !h.v.NextWake().Before(t0) {
		t.Fatalf("next check %v", h.v.NextWake())
	}
	if acts := h.v.Handle(Tick{Now: t0}); len(acts) != 1 || acts[0] != (CheckCameras{}) {
		t.Fatalf("%+v", acts)
	}
}
