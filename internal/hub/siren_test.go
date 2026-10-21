package hub

import (
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/engine"
)

// sirenOrders lists the commands sent to the siren so far.
func sirenOrders(r *rig) []published { return r.broker.on(contract.DeviceCmdTopic(site, devSiren)) }

// sirenAnswers answers a siren command as the Zigbee adapter does.
func sirenAnswers(r *rig, order published, status, reason string) {
	r.t.Helper()
	r.send(contract.DeviceAckTopic(site, devSiren), contract.TypeCommandAck,
		contract.CommandAckData{Ref: order.Env.ID, Status: status, Reason: reason})
}

func TestASirenOrderThatDidNotTakeIsSentAgainForWhatIsLeft(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	trigger(r)
	if len(sirenOrders(r)) != 1 {
		t.Fatalf("siren orders: %d", len(sirenOrders(r)))
	}
	// The adapter was down: it saw the order after its exp, 40 s into the cycle.
	r.clock.Advance(40 * time.Second)
	sirenAnswers(r, sirenOrders(r)[0], contract.CmdExpired, "")
	drivertest.WaitFor(t, "the order again", func() bool { r.settle(); return len(sirenOrders(r)) == 2 })
	cmd := dataOf[contract.DeviceCommandData](t, sirenOrders(r)[1])
	if cmd.Command != contract.CmdSirenOn || cmd.Args.DurationS != 260 || cmd.Exp == nil ||
		!cmd.Exp.Equal(r.clock.Now().Add(30*time.Second)) {
		t.Fatalf("%+v", cmd)
	}
	// An answer that says it worked changes nothing.
	sirenAnswers(r, sirenOrders(r)[1], contract.CmdAccepted, "")
	sirenAnswers(r, sirenOrders(r)[1], contract.CmdDone, "")
	r.settle()
	if len(sirenOrders(r)) != 2 {
		t.Fatalf("siren orders: %d", len(sirenOrders(r)))
	}
	r.validateAll()
}

func TestASirenThatNeverConfirmsIsToldAFewTimesThenReported(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	trigger(r)
	for range 3 + 2 {
		orders := sirenOrders(r)
		sirenAnswers(r, orders[len(orders)-1], contract.CmdFailed, "no_confirmation")
		r.settle()
	}
	if got := len(sirenOrders(r)); got != 1+3 {
		t.Fatalf("siren orders: %d, want the first and %d more", got, 3)
	}
	if !strings.Contains(r.logs.String(), "level=ERROR") || !strings.Contains(r.logs.String(), "siren") {
		t.Fatalf("the siren's failure must be reported:\n%s", r.logs.String())
	}
	mode := r.mode()
	if mode != engine.ModeTriggered {
		t.Fatalf("mode %s", mode)
	}
}

func TestAnAnswerToAnOlderSirenOrderIsIgnored(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.setCode("u_owner", "482915")
	trigger(r)
	on := sirenOrders(r)[0]
	r.order(contract.CmdDisarm, contract.CommandArgs{Code: "482915"}, contract.OriginApp, "u_owner")
	r.mustBe(engine.ModeDisarmed)
	r.settle()
	n := len(sirenOrders(r)) // the siren.off
	sirenAnswers(r, on, contract.CmdExpired, "")
	// An answer from another device, or about an order this hub never sent.
	r.send(contract.DeviceAckTopic(site, devDoor), contract.TypeCommandAck, contract.CommandAckData{Ref: on.Env.ID, Status: contract.CmdFailed})
	r.send(contract.DeviceAckTopic(site, devSiren), contract.TypeCommandAck, contract.CommandAckData{Ref: "01NOSUCHORDER", Status: contract.CmdFailed})
	r.settle()
	if got := len(sirenOrders(r)); got != n {
		t.Fatalf("siren orders: %d, want %d: the alarm is over", got, n)
	}
}

// A device event handled while the retained states still arrive already tells the
// siren where the cycle is: the boot that follows must not tell it again.
func TestTheBootDoesNotRepeatASirenOrderOfTheBootWindow(t *testing.T) {
	first := newRig(t, nil)
	first.start()
	trigger(first) // at t0+2m: cycle 1 until +7m, pause until +9m, cycle 2 until +14m
	first.stop()

	second := newRig(t, first.store, func(c *Config) { c.BootWindow = 300 * time.Millisecond })
	second.clock.Advance(10 * time.Minute) // in cycle 2, four minutes left
	second.startWithoutWaiting()
	second.device(devDoor, contract.CapContact, "closed")
	second.clock.PassTimer(t, 300*time.Millisecond)
	drivertest.WaitFor(t, "the boot", func() bool { return len(second.broker.ofType(contract.TypeInstallMode)) > 0 })
	second.settle()
	var on []contract.DeviceCommandData
	for _, o := range sirenOrders(second) {
		if cmd := dataOf[contract.DeviceCommandData](t, o); cmd.Command == contract.CmdSirenOn {
			on = append(on, cmd)
		}
	}
	if len(on) != 1 || on[0].Args.DurationS != 240 {
		t.Fatalf("siren.on orders: %+v, want one for 240 s", on)
	}
}
