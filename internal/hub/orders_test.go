package hub

import (
	"encoding/json"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
	"safehome/internal/schematest"
)

// The broker keeps the hub's session for a day: what was sent while the hub was down
// arrives when it comes back. An order typed hours ago must not run now.

func TestAKeypadPressHeldByTheBrokerIsNotCarriedOutLater(t *testing.T) {
	r := newRig(t, nil)
	r.withInstaller()
	r.start()
	r.keypadAt(r.clock.Now().Add(-2*time.Hour), contract.ValArmNow, "")
	r.waitAcks(1)
	if a := lastAck(t, r); a.Status != contract.AckExpired || r.mode() != engine.ModeDisarmed {
		t.Fatalf("%+v in mode %s: an old press must not arm", a, r.mode())
	}
	// Not with a code either: it is not checked, and an old installer code opens nothing.
	r.keypadAt(r.clock.Now().Add(-2*time.Hour), contract.ValDisarm, installerCode)
	r.waitAcks(2)
	if a := lastAck(t, r); a.Status != contract.AckExpired || r.lastInstall().Status == contract.InstallOpen {
		t.Fatalf("%+v, install %+v", a, r.lastInstall())
	}
	// A press of a few seconds ago counts, and so does one from a clock ahead of the hub's.
	r.keypadAt(r.clock.Now().Add(-20*time.Second), contract.ValArm, "")
	r.mustBe(engine.ModeArming)
	r.keypadAt(r.clock.Now().Add(time.Hour), contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)
	if len(eventsOf(r, contract.TypeAlarmAuthFailed)) != 0 {
		t.Fatal("an expired order counted as a wrong code")
	}
}

func TestAnOldAppOrderWithoutExpiryIsNotCarriedOut(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	old := r.clock.Now().Add(-2 * time.Hour)
	r.sendAt(old, contract.AlarmCmdTopic(site), contract.TypeAlarmCommand,
		contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp, User: "u_owner"})
	r.waitAcks(1)
	if a := lastAck(t, r); a.Status != contract.AckExpired || r.mode() != engine.ModeDisarmed {
		t.Fatalf("%+v in mode %s", a, r.mode())
	}
	// Its own expiry, when it has one, decides.
	r.sendAt(old, contract.AlarmCmdTopic(site), contract.TypeAlarmCommand,
		contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp, User: "u_owner",
			Exp: contract.TSPtr(r.clock.Now().Add(time.Minute))})
	r.mustBe(engine.ModeArming)
	r.validateAll()
}

// The engine's loop never lets time go back; the hub's clock may step back (a
// correction). A fresh order, stamped by the corrected clock, must not be taken for
// an old one: a normal entry would otherwise end in a full alarm.
func TestAFreshOrderAfterTheClockSteppedBackIsCarriedOut(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.clock.Advance(10 * time.Minute)
	r.settle() // the engine's loop has seen the later time
	r.clock.Advance(-5 * time.Minute)
	r.keypad(contract.ValArm, "")
	r.waitAcks(1)
	if a := lastAck(t, r); a.Status != contract.AckDone {
		t.Fatalf("%+v: a key just pressed must count", a)
	}
	r.mustBe(engine.ModeArming)
	// An old press is still old, on the same clock.
	r.keypadAt(r.clock.Now().Add(-2*time.Minute), contract.ValArmNow, "")
	r.waitAcks(2)
	if a := lastAck(t, r); a.Status != contract.AckExpired {
		t.Fatalf("%+v", a)
	}
}

// The cloud checks an order against the contract's schema before it sends it: the
// schema's form of a code (alarm-command.json, args.code) is the hub's own. A code
// the schema let through but the hub cannot recognise would be counted as a wrong
// one; a code the hub recognises but the schema refused could not be typed in the app.
func TestTheOrderSchemaTakesTheCodesTheHubRecognises(t *testing.T) {
	codes := []string{"", "48a915", "4829 ", " 4829", "4829\n", "-4829", "+4829", "4829.0", "0x4829",
		"\uff14\uff18\uff12\uff19", "\u0664\u0668\u0662\u0669\u0664\u0668"} // full-width and Arabic-Indic digits
	for n := 1; n <= 12; n++ {
		codes = append(codes, "482915736104"[:n])
	}
	format := DefaultConfig(site, devHub, devSiren).Access
	for _, code := range codes {
		raw, err := json.Marshal(map[string]any{"command": contract.CmdDisarm, "args": map[string]string{"code": code}, "origin": contract.OriginApp})
		if err != nil {
			t.Fatal(err)
		}
		inSchema, byHub := schematest.Check("alarm-command.json", raw) == nil, format.WellFormed(code)
		if inSchema != byHub {
			t.Errorf("code %q: the schema takes it %t, the hub %t", code, inSchema, byHub)
		}
	}
}

// slowCode sets the owner's code with a hash as slow as on the hub's board, where a
// check takes about a second: long enough to see what happens meanwhile.
func (r *rig) slowCode(code string) {
	r.t.Helper()
	slow := testConfig().Access
	slow.Iterations = 1_000_000
	reg, err := openCodes(slow, r.store)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := reg.Set("u_owner", code); err != nil {
		r.t.Fatal(err)
	}
	if err := r.hub.ReloadCodes(); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) armedNow() {
	r.t.Helper()
	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)
}

// The right code typed in the last moment of the entry delay is checked after its
// end: the alarm must not go off meanwhile, nor alert anyone.
func TestARightCodeTypedAtTheEndOfTheEntryDelayIsNotTooLate(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.slowCode("482915")
	r.armedNow()
	r.device(devDoor, contract.CapContact, "open")
	r.mustBe(engine.ModeEntryDelay)
	r.clock.Advance(44500 * time.Millisecond)
	r.keypad(contract.ValDisarm, "482915")
	r.settle()                   // the engine knows a code is being checked
	r.clock.Advance(time.Second) // the end of the delay passes during the check
	r.mustBe(engine.ModeDisarmed)
	if n := len(eventsOf(r, contract.TypeAlarmTriggered)) + len(eventsOf(r, contract.TypeAlertRequest)); n != 0 {
		t.Fatalf("%d alarm message(s) for a code typed in time", n)
	}
}

// An order without a code pressed right after one being checked waits for it: the
// engine takes them in the order they came.
func TestAnOrderWithoutACodeWaitsForTheOneBeingChecked(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.slowCode("482915")
	r.armedNow()
	r.keypad(contract.ValDisarm, "482915")
	r.keypad(contract.ValArm, "")
	r.waitAcks(4)
	r.mustBe(engine.ModeArming) // disarmed, then armed again
}
