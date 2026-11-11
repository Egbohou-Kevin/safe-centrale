package health

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func (h *harness) updating(until, made time.Time) []Action {
	return h.m.Handle(Updating{Now: h.now, Until: until, Made: made})
}

func (h *harness) updatingUntil() time.Time { return h.m.Status().UpdatingUntil }

// An announced update is in the status until its time, then no longer, with no input
// but the tick NextWake asks for.
func TestAnUpdateIsAnnouncedUntilItsTime(t *testing.T) {
	h := newHarness(t)
	until := t0.Add(15 * time.Minute)
	none(t, h.updating(until, t0))
	if got := h.updatingUntil(); !got.Equal(until) {
		t.Fatalf("UpdatingUntil %v, want %v", got, until)
	}
	if got := h.m.NextWake(); !got.Equal(until) {
		t.Fatalf("NextWake %v, want the end of the announcement %v", got, until)
	}
	h.advance(15*time.Minute - time.Second)
	if got := h.updatingUntil(); !got.Equal(until) {
		t.Fatalf("UpdatingUntil %v a second before its end", got)
	}
	none(t, h.advance(time.Second))
	if got := h.updatingUntil(); !got.IsZero() {
		t.Fatalf("UpdatingUntil %v after its end, want none", got)
	}
}

// The update script withdraws its announcement once the update is over.
func TestAWithdrawnUpdateIsNoLongerAnnounced(t *testing.T) {
	h := newHarness(t)
	h.updating(t0.Add(20*time.Minute), t0)
	h.advance(5 * time.Minute)
	h.updating(time.Time{}, time.Time{})
	if got := h.updatingUntil(); !got.IsZero() {
		t.Fatalf("UpdatingUntil %v once withdrawn, want none", got)
	}
}

// An announcement never lasts more than 20 minutes after it was made, nor after it was
// received; one already over, or made long ago, is none.
func TestAnUpdateIsNeverAnnouncedForMoreThanTwentyMinutes(t *testing.T) {
	for _, c := range []struct {
		name        string
		until, made time.Time
		want        time.Time
	}{
		{"an hour asked", t0.Add(time.Hour), t0, t0.Add(20 * time.Minute)},
		{"made 15 minutes ago", t0.Add(20 * time.Minute), t0.Add(-15 * time.Minute), t0.Add(5 * time.Minute)},
		{"made at an unknown time", t0.Add(time.Hour), time.Time{}, t0.Add(20 * time.Minute)},
		{"made later than now (a clock behind)", t0.Add(time.Hour), t0.Add(time.Hour), t0.Add(20 * time.Minute)},
		{"already over", t0.Add(-time.Minute), t0.Add(-10 * time.Minute), time.Time{}},
		{"made 25 minutes ago", t0.Add(time.Minute), t0.Add(-25 * time.Minute), time.Time{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.updating(c.until, c.made)
			if got := h.updatingUntil(); !got.Equal(c.want) {
				t.Fatalf("UpdatingUntil %v, want %v", got, c.want)
			}
		})
	}
}

// The announcement survives a restart while it holds (the hub restarts during an
// update), and is dropped at the first input once its time has passed.
func TestAnAnnouncedUpdateSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	until := t0.Add(10 * time.Minute)
	h.updating(until, t0)
	restart := func(at time.Time) {
		b, err := json.Marshal(h.m.Snapshot())
		if err != nil {
			t.Fatal(err)
		}
		var snap Snapshot
		if err := json.Unmarshal(b, &snap); err != nil {
			t.Fatal(err)
		}
		m, err := New(h.cfg, func() string { h.ids++; return fmt.Sprintf("alert-%d", h.ids) }, at)
		if err != nil {
			t.Fatal(err)
		}
		m.Restore(snap)
		h.m, h.now = m, at
	}
	restart(t0.Add(2 * time.Minute))
	if got := h.updatingUntil(); !got.Equal(until) {
		t.Fatalf("UpdatingUntil %v after a restart, want %v", got, until)
	}
	if got := h.m.NextWake(); !got.Equal(until) {
		t.Fatalf("NextWake %v after a restart, want %v", got, until)
	}
	restart(t0.Add(11 * time.Minute))
	h.advance(0)
	if got := h.updatingUntil(); !got.IsZero() {
		t.Fatalf("UpdatingUntil %v restored after its end, want none", got)
	}
	// A state saved before the announcement existed says none.
	var old Snapshot
	if err := json.Unmarshal([]byte(`{"mains_lost":false}`), &old); err != nil {
		t.Fatal(err)
	}
	if !old.UpdatingUntil.IsZero() {
		t.Fatalf("%v", old.UpdatingUntil)
	}
	if b, _ := json.Marshal(Snapshot{}); strings.Contains(string(b), "updating_until") {
		t.Fatalf("a state without an announcement saves one: %s", b)
	}
}

// During an update the alerts go on as without it: the announcement changes only what
// is shown.
func TestAnAnnouncedUpdateChangesNoAlert(t *testing.T) {
	h := newHarness(t)
	h.zigbee(true)
	h.updating(t0.Add(20*time.Minute), t0)
	h.zigbee(false)
	if a := one(t, h.advance(h.cfg.ZigbeeDownAfter)); a.Kind != AlertZigbeeDown {
		t.Fatalf("%+v, want zigbee_down during the update", a)
	}
}
