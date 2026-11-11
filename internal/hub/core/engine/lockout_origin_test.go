package engine

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// Since 29 October 2026 the wrong codes lock their own origin: the keypad on one side,
// the app (every other origin) on the other. A cloud that seals any code, compromised
// or wrong, must not keep the staff from disarming at the keypad.

func fromApp(c *Command) { c.Origin = OriginApp }

// lockFrom enters LockoutAttempts wrong codes from the origin and returns the last
// failure, the one that locks.
func (h *harness) lockFrom(o Origin) AuthFailed {
	h.t.Helper()
	var af AuthFailed
	for range h.cfg.LockoutAttempts {
		af = h.badAt(0, o)
	}
	if af.Origin != o || af.Attempts != h.cfg.LockoutAttempts || !af.LockedUntil.Equal(h.now.Add(h.cfg.LockoutDuration)) {
		h.t.Fatalf("%s did not lock: %+v", o, af)
	}
	return af
}

func TestTheAppsLockoutLeavesTheKeypadFree(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	h.lockFrom(OriginApp)
	if st := h.e.State(h.now); st.LockedUntil.IsZero() || !st.KeypadLockedUntil.IsZero() {
		t.Fatalf("the app's lockout is the published one, the keypad free: %+v", st)
	}
	if r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner"), fromApp)); r.Reason != ReasonLockedOut {
		t.Fatalf("the app disarmed while locked: %+v", r)
	}
	h.mustBe(ModeTriggered)
	if r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner"))); r.Status != StatusDone {
		t.Fatalf("the keypad locked by the app's wrong codes: %+v", r)
	}
	h.mustBe(ModeDisarmed)
}

func TestTheKeypadsLockoutLeavesTheAppFree(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	h.lockFrom(OriginKeypad)
	if st := h.e.State(h.now); !st.LockedUntil.IsZero() || st.KeypadLockedUntil.IsZero() {
		t.Fatalf("the keypad's lockout is not published, the app free: %+v", st)
	}
	if r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner"))); r.Reason != ReasonLockedOut {
		t.Fatalf("the keypad disarmed while locked: %+v", r)
	}
	h.mustBe(ModeTriggered)
	if r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner"), fromApp)); r.Status != StatusDone {
		t.Fatalf("the app locked by the keypad's wrong codes: %+v", r)
	}
	h.mustBe(ModeDisarmed)
}

// Silencing the siren needs the same right as disarming: the app's lockout does not
// keep the keypad from it either.
func TestTheAppsLockoutLeavesTheKeypadSilencing(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	h.lockFrom(OriginApp)
	if r := replyOf(t, h.cmd(CmdSilence, goodCode("u_owner"), fromApp)); r.Reason != ReasonLockedOut {
		t.Fatalf("the app silenced while locked: %+v", r)
	}
	if r := replyOf(t, h.cmd(CmdSilence, goodCode("u_owner"))); r.Status != StatusDone {
		t.Fatalf("the keypad kept from silencing by the app's wrong codes: %+v", r)
	}
}

// Each origin counts its own: two from the app and one from the keypad lock neither,
// but the site's count since the last good code alerts the contacts all the same.
func TestWrongCodesFromBothOriginsAlertWithoutLocking(t *testing.T) {
	h := newHarness(t)
	h.armed()
	for i := 1; i <= 2; i++ {
		as := h.cmd(CmdDisarm, badCode, fromApp)
		if af := onlyEvent[AuthFailed](t, as); af.Attempts != i || len(alerts(as)) != 0 {
			t.Fatalf("app's wrong code %d: %+v, alerts %v", i, af, alerts(as))
		}
	}
	as := h.cmd(CmdDisarm, badCode)
	af, al := onlyEvent[AuthFailed](t, as), alerts(as)
	if af.Origin != OriginKeypad || af.Attempts != 1 || !af.LockedUntil.IsZero() {
		t.Fatalf("the keypad's first wrong code: %+v", af)
	}
	if len(al) != 1 || al[0].Kind != AlertAuthFailed {
		t.Fatalf("the third wrong code of the site did not alert: %v", al)
	}
	if st := h.e.State(h.now); !st.LockedUntil.IsZero() || !st.KeypadLockedUntil.IsZero() {
		t.Fatalf("locked: %+v", st)
	}
	for _, o := range []Origin{OriginApp, OriginKeypad} {
		h.armed()
		if r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner"), func(c *Command) { c.Origin = o })); r.Status != StatusDone {
			t.Fatalf("%s: %+v", o, r)
		}
	}
}

// A good code at the keypad starts over the keypad's count and the site's, not the
// app's lockout: the app stays locked for its whole duration.
func TestAGoodCodeAtTheKeypadLeavesTheAppLocked(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.badAt(0, OriginKeypad)
	locked := h.lockFrom(OriginApp)
	if r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner"))); r.Status != StatusDone {
		t.Fatalf("the keypad: %+v", r)
	}
	if s := h.e.Snapshot(); len(s.KeypadFailures) != 0 || s.WrongCodes != 0 || !s.LockedUntil.Equal(locked.LockedUntil) {
		t.Fatalf("after a good code at the keypad: %+v", s)
	}
	h.armed()
	if r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner"), fromApp)); r.Reason != ReasonLockedOut {
		t.Fatalf("a good code at the keypad lifted the app's lockout: %+v", r)
	}
	h.now = locked.LockedUntil
	if r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner"), fromApp)); r.Status != StatusDone {
		t.Fatalf("the app after its lockout: %+v", r)
	}
}

// Every origin but the keypad counts as the app's.
func TestEveryOriginButTheKeypadCountsAsTheApp(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.badAt(0, OriginApp)
	h.badAt(0, Origin("cloud"))
	if af := h.badAt(0, OriginSchedule); af.Attempts != 3 || af.LockedUntil.IsZero() {
		t.Fatalf("other origins do not count with the app: %+v", af)
	}
	if r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner"))); r.Status != StatusDone {
		t.Fatalf("the keypad: %+v", r)
	}
}

// The snapshot keeps the fields it had: a program from before the keypad's own
// lockout, brought back by a rollback, reads the app's and ignores the keypad's. A
// state saved before, read by this program, gives its lockout to the app and leaves
// the keypad free.
func TestTheSnapshotKeepsItsOldLockoutFields(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.lockFrom(OriginKeypad)
	h.badAt(0, OriginApp)
	snap := h.e.Snapshot()
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		Failures    []time.Time `json:"failures"`
		LockedUntil time.Time   `json:"locked_until"`
		WrongCodes  int         `json:"wrong_codes"`
	}
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old.Failures, snap.Failures) || !old.LockedUntil.Equal(snap.LockedUntil) || old.WrongCodes != 4 {
		t.Fatalf("the old fields as read by the old program: %+v, snapshot %+v", old, snap)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["keypad_locked_until"]; !ok {
		t.Fatalf("the keypad's lockout is not saved: %s", b)
	}

	// A state saved before: one site lockout, read again by this program.
	h = newHarness(t)
	h.armed()
	h.lockFrom(OriginApp)
	b, err = json.Marshal(h.e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	fields = nil // Unmarshal would add to the map read above
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["keypad_locked_until"]; ok {
		t.Fatalf("a free keypad is written: %s", b)
	}
	h.e = restart(t, h) // through the JSON, as the hub reads its state file
	h.e.Boot(h.now, Booted{}, nil)
	if r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner"), fromApp)); r.Reason != ReasonLockedOut {
		t.Fatalf("the old lockout lost: %+v", r)
	}
	if r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner"))); r.Status != StatusDone {
		t.Fatalf("the keypad locked by an old state: %+v", r)
	}
}

// A clock set forward keeps both lockouts their real length; a clock set back ends
// both at most LockoutDuration from now.
func TestBothLockoutsFollowTheClock(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.lockFrom(OriginKeypad)
	h.lockFrom(OriginApp)
	h.advance(time.Minute)
	const step = 14 * time.Hour
	h.e.Stepped(step)
	h.now = h.now.Add(step)
	want := h.now.Add(h.cfg.LockoutDuration - time.Minute)
	if st := h.e.State(h.now); !st.LockedUntil.Equal(want) || !st.KeypadLockedUntil.Equal(want) {
		t.Fatalf("after a clock set forward: %+v, want both until %v", st, want)
	}

	h.now = h.now.Add(-2 * step) // set back, within the run
	h.advance(0)
	want = h.now.Add(h.cfg.LockoutDuration)
	if st := h.e.State(h.now); !st.LockedUntil.Equal(want) || !st.KeypadLockedUntil.Equal(want) {
		t.Fatalf("after a clock set back: %+v, want both until %v", st, want)
	}
	h.advance(h.cfg.LockoutDuration)
	for _, o := range []Origin{OriginApp, OriginKeypad} {
		if r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner"), func(c *Command) { c.Origin = o })); r.Status != StatusDone {
			t.Fatalf("%s still locked: %+v", o, r)
		}
		h.armed()
	}
}
