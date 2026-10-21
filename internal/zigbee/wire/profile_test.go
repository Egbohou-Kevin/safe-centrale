package wire_test

import (
	"path/filepath"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/zigbee"
	"safehome/internal/zigbee/wire"
	"safehome/internal/zigbeeconfig"
)

// The real keypad profile, through the core and the wire: the Develco keypad sends
// the code typed before any key, "arm" included. The contract takes a code only with
// disarm or silence, so the code of an arming is dropped, not the arming.
func TestTheKeypadArmsWithACodeTypedFirst(t *testing.T) {
	profiles, err := zigbeeconfig.LoadProfiles(filepath.Join("..", "..", "..", "config", "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := zigbee.New(zigbee.DefaultConfig(profiles), func() string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	n.Restore(zigbee.Snapshot{Devices: map[string]zigbee.Device{"dev_kp": {
		ID: "dev_kp", IEEE: "0x01", Model: "KEPZB-110", Profile: "develco.keypad.kepzb110",
	}}})
	now := time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

	press := func(fields map[string]any) contract.DeviceEventData {
		t.Helper()
		var events []contract.DeviceEventData
		for _, a := range n.Handle(zigbee.Report{Now: now, Name: "dev_kp", Fields: fields}) {
			ch, ok := a.(zigbee.Changed)
			if !ok {
				continue
			}
			out, err := wire.Changed(ch)
			if err != nil {
				t.Fatalf("%s: %v", ch.Value, err)
			}
			if out.Event != nil {
				events = append(events, *out.Event)
			}
		}
		if len(events) != 1 {
			t.Fatalf("want one event, got %v", events)
		}
		return events[0]
	}

	arm := press(map[string]any{"action": "arm_all_zones", "action_code": "482915"})
	if arm.Capability != contract.CapKeypad || arm.StringValue() != contract.ValArm || arm.Code != "" {
		t.Fatalf("arm: %v", arm)
	}
	disarm := press(map[string]any{"action": "disarm", "action_code": "482915"})
	if disarm.StringValue() != contract.ValDisarm || disarm.Code != "482915" {
		t.Fatalf("disarm: %v", disarm)
	}
	panic := press(map[string]any{"action": "emergency", "action_code": "482915"})
	if panic.Capability != contract.CapPanic || panic.Code != "" {
		t.Fatalf("panic: %v", panic)
	}
}
