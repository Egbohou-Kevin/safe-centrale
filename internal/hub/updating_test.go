package hub

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
)

// fakeAnnouncement stands for the update announcement deploy/update-hub.sh writes.
type fakeAnnouncement struct{ v atomic.Pointer[[2]time.Time] }

func (f *fakeAnnouncement) set(until, made time.Time) { f.v.Store(&[2]time.Time{until, made}) }

func (f *fakeAnnouncement) deps(d *Deps) {
	d.Updating = func() (time.Time, time.Time, error) {
		a := f.v.Load()
		if a == nil {
			return time.Time{}, time.Time{}, nil
		}
		return a[0], a[1], nil
	}
}

// updatingUntil is the updating_until of the last system.health published.
func updatingUntil(r *rig) *contract.Timestamp {
	h := r.broker.On(contract.HealthTopic(site))
	if len(h) == 0 {
		return nil
	}
	return dataOf[contract.HealthData](r.t, h[len(h)-1]).UpdatingUntil
}

// waitUpdating polls the announcement on the fake clock until system.health says
// want (nil: no update).
func waitUpdating(r *rig, what string, want time.Time) {
	r.t.Helper()
	drivertest.WaitFor(r.t, what, func() bool {
		r.clock.Advance(r.hub.cfg.UpdatingPoll)
		r.settle()
		got := updatingUntil(r)
		if want.IsZero() {
			return got == nil
		}
		return got != nil && got.Equal(want)
	})
}

// While an update is announced, system.health carries updating_until; once withdrawn,
// or past its time with nobody to withdraw it, it no longer does. The journal says each
// change: the update script reads it before it stops anything.
func TestSystemHealthSaysAnAnnouncedUpdate(t *testing.T) {
	var ann fakeAnnouncement
	until := t0.Add(15 * time.Minute)
	ann.set(until, t0)
	r := newRigWith(t, t0, nil, ann.deps)
	r.start()
	waitUpdating(r, "the update announced", until)
	if !strings.Contains(r.logs.String(), `msg="update announced" until=`+until.Format(time.RFC3339)) {
		t.Fatalf("the announcement is not in the journal:\n%s", r.logs.String())
	}

	ann.set(time.Time{}, time.Time{}) // the update script withdraws it
	waitUpdating(r, "the update withdrawn", time.Time{})
	if !strings.Contains(r.logs.String(), `msg="update no longer announced"`) {
		t.Fatalf("the withdrawal is not in the journal:\n%s", r.logs.String())
	}

	// Announced again, then left behind by an update cut short: it ends at its time.
	now := r.clock.Now()
	until = now.Add(time.Minute)
	ann.set(until, now)
	waitUpdating(r, "the update announced again", until)
	waitUpdating(r, "the announcement past its time", time.Time{})
	if !r.clock.Now().After(until.Add(-time.Second)) {
		t.Fatalf("the announcement ended at %v, before its time %v", r.clock.Now(), until)
	}
	r.validateAll()
}

// The hub restarts during an update (its new program, the reboots): an announcement
// still valid is said again from the start, one withdrawn meanwhile is not.
func TestAnAnnouncedUpdateIsSaidAgainAfterARestart(t *testing.T) {
	var ann fakeAnnouncement
	until := t0.Add(15 * time.Minute)
	ann.set(until, t0)
	store := drivertest.NewMemStore()
	r := newRigWith(t, t0, store, ann.deps)
	r.start()
	waitUpdating(r, "the update announced", until)
	r.stop()

	again := newRigWith(t, t0.Add(2*time.Minute), store, ann.deps)
	again.start()
	again.settle()
	h := again.broker.On(contract.HealthTopic(site))
	if first := dataOf[contract.HealthData](t, h[0]).UpdatingUntil; first == nil || !first.Equal(until) {
		t.Fatalf("first system.health after the restart says updating_until %v, want %v", first, until)
	}
	again.validateAll()
	again.stop()

	ann.set(time.Time{}, time.Time{})
	last := newRigWith(t, t0.Add(4*time.Minute), store, ann.deps)
	last.start()
	waitUpdating(last, "the withdrawn announcement gone after a restart", time.Time{})
}

// An announcement that cannot be read counts as none, and is logged; the alarm goes on.
func TestAnAnnouncementThatCannotBeReadIsNone(t *testing.T) {
	var broken atomic.Bool
	r := newRigWith(t, t0, nil, func(d *Deps) {
		d.Updating = func() (time.Time, time.Time, error) {
			if broken.Load() {
				return time.Time{}, time.Time{}, errors.New("garbled")
			}
			return t0.Add(10 * time.Minute), t0, nil
		}
	})
	r.start()
	waitUpdating(r, "the update announced", t0.Add(10*time.Minute))
	broken.Store(true)
	waitUpdating(r, "the unreadable announcement dropped", time.Time{})
	if !strings.Contains(r.logs.String(), "the update announcement cannot be read") {
		t.Fatalf("not logged:\n%s", r.logs.String())
	}
}
