package hub

import (
	"slices"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/engine"
)

func TestArmingWarnsOfSilentDevicesAndWeakBatteries(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.offline(devBack, time.Time{})
	r.device(devMotion, contract.CapBattery, "9")
	r.settle()

	ref := r.order(contract.CmdArm, contract.CommandArgs{}, contract.OriginApp, "u_owner")
	r.mustBe(engine.ModeArming) // never refused for a fault
	drivertest.WaitFor(t, "the reply", func() bool { return len(eventsOf(r, contract.TypeAlarmAck)) == 1 })
	r.settle()

	arming := dataOf[contract.ArmingData](t, eventsOf(r, contract.TypeAlarmArming)[0])
	if !slices.Equal(arming.Offline, []string{devBack}) || !slices.Equal(arming.LowBattery, []string{devMotion}) {
		t.Fatalf("arming event %+v", arming)
	}
	ack := dataOf[contract.AlarmAckData](t, eventsOf(r, contract.TypeAlarmAck)[0])
	if ack.Ref != ref || ack.Status != contract.AckDone ||
		!sameList(ack.Detail["offline"], devBack) || !sameList(ack.Detail["low_battery"], devMotion) {
		t.Fatalf("reply %+v", ack)
	}
	r.validateAll()
}

func TestArmingWithoutFaultsSaysNothingMore(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.offline(devBack, time.Time{})
	r.online(devBack) // back
	r.settle()
	r.order(contract.CmdArm, contract.CommandArgs{}, contract.OriginApp, "u_owner")
	drivertest.WaitFor(t, "the reply", func() bool { return len(eventsOf(r, contract.TypeAlarmAck)) == 1 })
	r.settle()
	arming := dataOf[contract.ArmingData](t, eventsOf(r, contract.TypeAlarmArming)[0])
	ack := dataOf[contract.AlarmAckData](t, eventsOf(r, contract.TypeAlarmAck)[0])
	if arming.Offline != nil || arming.LowBattery != nil || ack.Detail != nil {
		t.Fatalf("%+v %+v", arming, ack)
	}
}

func TestARefusedArmingCarriesOnlyItsReason(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.device(devMotion, contract.CapBattery, "9")
	r.device(devDoor, contract.CapContact, "open")
	r.settle()
	r.order(contract.CmdArm, contract.CommandArgs{}, contract.OriginApp, "u_owner")
	drivertest.WaitFor(t, "the reply", func() bool { return len(eventsOf(r, contract.TypeAlarmAck)) == 1 })
	ack := dataOf[contract.AlarmAckData](t, eventsOf(r, contract.TypeAlarmAck)[0])
	if ack.Status != contract.AckRefused || ack.Detail["low_battery"] != nil {
		t.Fatalf("%+v", ack)
	}
}

// sameList tells whether a decoded JSON value is the list of the one device.
func sameList(v any, dev string) bool {
	l, ok := v.([]any)
	return ok && len(l) == 1 && l[0] == dev
}
