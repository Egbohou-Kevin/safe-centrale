package hub

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
)

// heldLane holds a lane's worker: each order it takes waits for release, so that the
// lane stays busy whatever the speed of the hash.
type heldLane struct {
	t       *testing.T
	taken   chan struct{}
	release func()
}

// holdLane holds the lane's worker until release (or the hub's end). Call it before
// start: the worker reads its check when it runs.
func holdLane(r *rig, l *lane) *heldLane {
	r.t.Helper()
	taken, gate := make(chan struct{}, 64), make(chan struct{})
	var once sync.Once
	h := &heldLane{t: r.t, taken: taken, release: func() { once.Do(func() { close(gate) }) }}
	check, life := l.check, r.hub.life
	l.check = func(o order) {
		select {
		case taken <- struct{}{}:
		default:
		}
		select {
		case <-gate:
			check(o)
		case <-life.Done():
		}
	}
	return h
}

// waitTaken waits until the worker holds an order.
func (h *heldLane) waitTaken() {
	h.t.Helper()
	select {
	case <-h.taken:
	case <-time.After(10 * time.Second):
		h.t.Fatal("the lane's worker took no order")
	}
}

// appWrongCodes sends n app disarms with wrong codes.
func (r *rig) appWrongCodes(n int) {
	r.t.Helper()
	for i := 0; i < n; i++ {
		r.order(contract.CmdDisarm, contract.CommandArgs{Code: fmt.Sprintf("%06d", 100000+i*7)}, contract.OriginApp, "u_owner")
	}
}

// fillAppLane makes the app lane busy and full: one order held by its worker, the
// queue full behind it, then overflow orders refused.
func (r *rig) fillAppLane(held *heldLane, overflow int) {
	r.t.Helper()
	r.appWrongCodes(1)
	held.waitTaken()
	r.appWrongCodes(r.hub.cfg.AuthQueue + overflow)
}

// authFailedOrigins lists the origins of the published alarm.auth_failed.
func authFailedOrigins(r *rig) map[string]int {
	r.t.Helper()
	n := map[string]int{}
	for _, p := range eventsOf(r, contract.TypeAlarmAuthFailed) {
		n[dataOf[contract.AuthFailedData](r.t, p).Origin]++
	}
	return n
}

// lockouts reads the app's and the keypad's lockouts from the engine.
func (r *rig) lockouts() (app, keypad time.Time) {
	r.t.Helper()
	if err := r.hub.alarm.loop.Do(context.Background(), func(now time.Time) []engine.Action {
		st := r.hub.alarm.eng.State(now)
		app, keypad = st.LockedUntil, st.KeypadLockedUntil
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
	return app, keypad
}

// The app's lane busy and full: a right code at the keypad goes through at once, and
// nothing is counted against the keypad.
func TestAKeypadCodeDoesNotWaitBehindTheAppsChecks(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.AuthQueue = 2 })
	app := holdLane(r, r.hub.appChecks)
	r.start()
	t.Cleanup(app.release)
	r.setCode("u_owner", "482915")
	r.armedNow()
	r.fillAppLane(app, 3)
	r.keypad(contract.ValDisarm, "482915")
	r.mustBe(engine.ModeDisarmed)
	if n := authFailedOrigins(r)[contract.OriginKeypad]; n != 0 {
		t.Fatalf("%d auth_failed from the keypad for a right code", n)
	}
}

// A flood from the app that overflows its lane is counted against the app only: the
// keypad is not locked, and still disarms.
func TestAFloodFromTheAppLocksTheAppOnly(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.AuthQueue = 2 })
	app := holdLane(r, r.hub.appChecks)
	r.start()
	t.Cleanup(app.release)
	r.setCode("u_owner", "482915")
	r.armedNow()
	r.fillAppLane(app, 20)
	r.settle()
	origins := authFailedOrigins(r)
	if origins[contract.OriginApp] == 0 || len(origins) != 1 {
		t.Fatalf("auth_failed by origin %v: the refused app orders must count for the app only", origins)
	}
	appLock, keypadLock := r.lockouts()
	if appLock.IsZero() || !keypadLock.IsZero() {
		t.Fatalf("app locked until %v, keypad until %v: only the app must be locked", appLock, keypadLock)
	}
	r.keypad(contract.ValDisarm, "482915")
	r.mustBe(engine.ModeDisarmed)
}

// The app's lane busy during an entry delay: a right code typed at the keypad in its
// last moment disarms, and the alarm does not go off.
func TestAKeypadCodeEndsTheEntryDelayWhileTheAppIsChecking(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.AuthQueue = 2 })
	app := holdLane(r, r.hub.appChecks)
	r.start()
	t.Cleanup(app.release)
	r.setCode("u_owner", "482915")
	r.armedNow()
	r.device(devDoor, contract.CapContact, "open")
	r.mustBe(engine.ModeEntryDelay)
	r.fillAppLane(app, 0) // full, nothing refused: the app's wrong codes would alert
	r.clock.Advance(44500 * time.Millisecond)
	r.keypad(contract.ValDisarm, "482915")
	r.settle()
	r.clock.Advance(time.Second) // the end of the delay
	r.mustBe(engine.ModeDisarmed)
	r.clock.Advance(engine.CodeCheckHold)
	r.settle()
	if n := len(eventsOf(r, contract.TypeAlarmTriggered)) + len(eventsOf(r, contract.TypeAlertRequest)); n != 0 {
		t.Fatalf("%d alarm message(s) for a code typed in time", n)
	}
}

// Within the keypad's lane the order is kept while the app checks: a "disarm" with its
// code, then an "arm" without one, reach the engine in that order.
func TestTheKeypadsOrdersKeepTheirOrderWhileTheAppIsChecking(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.AuthQueue = 2 })
	app, keypad := holdLane(r, r.hub.appChecks), holdLane(r, r.hub.keypadChecks)
	r.start()
	t.Cleanup(app.release)
	t.Cleanup(keypad.release)
	r.setCode("u_owner", "482915")
	r.armedNow()
	r.fillAppLane(app, 0)
	r.keypad(contract.ValDisarm, "482915")
	keypad.waitTaken()
	r.keypad(contract.ValArm, "")
	keypad.release()
	r.mustBe(engine.ModeArming) // disarmed, then armed again
}
