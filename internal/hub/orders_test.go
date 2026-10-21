package hub

import (
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
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
