package maint

import (
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)

func newCore(t *testing.T, edit ...func(*Config)) *Service {
	t.Helper()
	cfg := DefaultConfig()
	for _, e := range edit {
		e(&cfg)
	}
	v, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func open(v *Service, now time.Time, session string, length time.Duration) []Action {
	return v.Handle(Open{Now: now, Session: session, User: "kevin", Length: length, Exp: now.Add(time.Minute)})
}

func TestASessionOpensThenClosesWhenItsTimeIsOver(t *testing.T) {
	v := newCore(t)
	if !v.NextWake().IsZero() {
		t.Fatal("nothing to do")
	}
	acts := open(v, t0, "m1", 3*time.Hour) // asked too long: an hour at most
	want := []Action{Start{Session: "m1"}, Report{Session: "m1", User: "kevin", Status: Opening, Until: t0.Add(time.Hour)}}
	if !reflect.DeepEqual(acts, want) {
		t.Fatalf("%+v", acts)
	}
	if acts := open(v, t0, "m1", time.Hour); acts != nil {
		t.Fatal("the same order again")
	}
	acts = v.Handle(Up{Now: t0.Add(2 * time.Second), Session: "m1"})
	if !reflect.DeepEqual(acts, []Action{Report{Session: "m1", User: "kevin", Status: IsOpen, Until: t0.Add(time.Hour)}}) {
		t.Fatalf("%+v", acts)
	}
	if v.Handle(Up{Now: t0.Add(3 * time.Second), Session: "m1"}) != nil {
		t.Fatal("open once")
	}
	if !v.NextWake().Equal(t0.Add(time.Hour)) {
		t.Fatalf("%v", v.NextWake())
	}
	if v.Handle(Tick{Now: t0.Add(59 * time.Minute)}) != nil {
		t.Fatal("not yet")
	}
	acts = v.Handle(Tick{Now: t0.Add(time.Hour)})
	want = []Action{Stop{Session: "m1"}, Report{Session: "m1", User: "kevin", Status: Closed, Reason: Timeout, Duration: time.Hour - 2*time.Second}}
	if !reflect.DeepEqual(acts, want) {
		t.Fatalf("%+v", acts)
	}
	if v.Snapshot() != (Snapshot{}) || v.Handle(Tick{Now: t0.Add(2 * time.Hour)}) != nil {
		t.Fatal("over")
	}
}

func TestRefusals(t *testing.T) {
	v := newCore(t)
	open(v, t0, "m1", time.Hour)
	if acts := open(v, t0, "m2", time.Hour); !reflect.DeepEqual(acts, []Action{Report{Session: "m2", User: "kevin", Status: Failed, Reason: Busy}}) {
		t.Fatalf("%+v", acts)
	}
	if acts := v.Handle(Open{Now: t0, Session: "m3", User: "u", Length: time.Hour, Exp: t0}); acts[0].(Report).Reason != Expired {
		t.Fatalf("%+v", acts)
	}
	off := newCore(t, func(c *Config) { c.Enabled = false })
	if acts := open(off, t0, "m1", time.Hour); acts[0].(Report).Reason != Disabled {
		t.Fatalf("%+v", acts)
	}
	if v.Handle(Close{Now: t0, Session: "other"}) != nil || v.Handle(Close{Now: t0}) != nil {
		t.Fatal("not this session")
	}
	for _, bad := range []Config{{MaxLength: 30 * time.Second, RetryAfter: time.Second}, {MaxLength: 2 * time.Hour, RetryAfter: time.Second}, {MaxLength: time.Hour}} {
		if _, err := New(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestTheCloudClosesIt(t *testing.T) {
	v := newCore(t)
	open(v, t0, "m1", time.Hour)
	acts := v.Handle(Close{Now: t0.Add(time.Minute), Session: "m1"})
	if !reflect.DeepEqual(acts, []Action{Stop{Session: "m1"}, Report{Session: "m1", User: "kevin", Status: Closed, Reason: Stopped}}) {
		t.Fatalf("%+v: never up, no duration", acts)
	}
}

func TestABastionThatDoesNotLetTheHubIn(t *testing.T) {
	v := newCore(t)
	open(v, t0, "m1", time.Hour)
	acts := v.Handle(Down{Now: t0.Add(15 * time.Second), Session: "m1"})
	if !reflect.DeepEqual(acts, []Action{Report{Session: "m1", User: "kevin", Status: Failed, Reason: Unreachable}}) {
		t.Fatalf("%+v", acts)
	}
	if v.Snapshot().Session != "" {
		t.Fatal("over")
	}
	if acts := open(v, t0.Add(time.Minute), "m2", time.Hour); len(acts) != 2 {
		t.Fatal("a new session may start")
	}
}

func TestABrokenTunnelIsOpenedAgain(t *testing.T) {
	v := newCore(t)
	open(v, t0, "m1", time.Hour)
	v.Handle(Up{Now: t0, Session: "m1"})
	if acts := v.Handle(Down{Now: t0.Add(10 * time.Minute), Session: "m1"}); acts != nil {
		t.Fatalf("%+v: nothing said, it is tried again", acts)
	}
	if v.Handle(Down{Now: t0.Add(10 * time.Minute), Session: "m1"}) != nil {
		t.Fatal("not running")
	}
	if !v.NextWake().Equal(t0.Add(10*time.Minute + 10*time.Second)) {
		t.Fatalf("%v", v.NextWake())
	}
	if acts := v.Handle(Tick{Now: t0.Add(10*time.Minute + 10*time.Second)}); !reflect.DeepEqual(acts, []Action{Start{Session: "m1"}}) {
		t.Fatalf("%+v", acts)
	}
	if v.Handle(Up{Now: t0.Add(11 * time.Minute), Session: "m1"}) != nil {
		t.Fatal("open was said already")
	}
	// It breaks again just before the end: the end wins.
	v.Handle(Down{Now: t0.Add(time.Hour - 5*time.Second), Session: "m1"})
	if !v.NextWake().Equal(t0.Add(time.Hour)) {
		t.Fatalf("%v", v.NextWake())
	}
	acts := v.Handle(Tick{Now: t0.Add(time.Hour)})
	if len(acts) != 1 || acts[0].(Report).Reason != Timeout {
		t.Fatalf("%+v: nothing to stop", acts)
	}
}

func TestASessionSurvivesARestart(t *testing.T) {
	v := newCore(t)
	open(v, t0, "m1", time.Hour)
	v.Handle(Up{Now: t0, Session: "m1"})
	saved := v.Snapshot()

	v2 := newCore(t)
	v2.Restore(saved)
	if acts := v2.Handle(Boot{Now: t0.Add(time.Minute)}); !reflect.DeepEqual(acts, []Action{Start{Session: "m1"}}) {
		t.Fatalf("%+v", acts)
	}
	v3 := newCore(t)
	v3.Restore(saved)
	acts := v3.Handle(Boot{Now: t0.Add(2 * time.Hour)})
	if len(acts) != 1 || acts[0].(Report).Reason != Timeout || acts[0].(Report).Duration != 2*time.Hour {
		t.Fatalf("%+v", acts)
	}
	if newCore(t).Handle(Boot{Now: t0}) != nil {
		t.Fatal("nothing saved")
	}
}
