package wire

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/schematest"
	"safehome/internal/zigbee"
)

var t0 = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

func TestAChangeMakesAnEventAndAState(t *testing.T) {
	out, err := Changed(zigbee.Changed{DeviceID: "dev_door", Capability: "contact", Value: "open", Since: t0, Notify: true, State: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Event == nil || string(out.Event.Value) != `"open"` || out.Event.Since != nil {
		t.Fatalf("event %+v", out.Event)
	}
	if out.State == nil || out.State.Since == nil || !out.State.Since.Equal(t0) {
		t.Fatalf("state %+v", out.State)
	}
	schematest.Validate(t, "device-event.json", *out.Event)
	schematest.Validate(t, "device-event.json", *out.State)

	out, _ = Changed(zigbee.Changed{DeviceID: "d", Capability: "battery", Value: "82", Numeric: true, State: true})
	if out.Event != nil || string(out.State.Value) != `82` {
		t.Fatalf("a state alone, as a number: %+v", out)
	}
}

func TestAKeypadCodeGoesInTheEventNeverInTheState(t *testing.T) {
	out, err := Changed(zigbee.Changed{DeviceID: "dev_kp", Capability: "keypad", Value: "disarm", Code: "482915", Notify: true, State: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Event.Code != "482915" || out.State.Code != "" {
		t.Fatalf("%+v", out)
	}
	_, err = Changed(zigbee.Changed{DeviceID: "dev_kp", Capability: "keypad", Value: "disarm", Code: "48a915", Notify: true})
	if err == nil || strings.Contains(err.Error(), "48a915") {
		t.Fatalf("refused, without the code in the error: %v", err)
	}
}

func TestValuesTheContractRefusesAreErrors(t *testing.T) {
	for _, c := range []zigbee.Changed{
		{DeviceID: "d", Capability: "contact", Value: "ajar"},
		{DeviceID: "d", Capability: "teleport", Value: "on"},
		{DeviceID: "d", Capability: "battery", Value: "lots", Numeric: true},
		{DeviceID: "d", Capability: "battery", Value: "120", Numeric: true},
	} {
		if _, err := Changed(c); err == nil {
			t.Errorf("%+v: accepted", c)
		}
	}
}

func TestPresence(t *testing.T) {
	heard := t0.Add(-4 * time.Hour)
	out := Presence(zigbee.Presence{DeviceID: "d", Since: t0, Heard: heard})
	if string(out.Event.Value) != `"offline"` || out.Event.Capability != contract.CapAvailability || !out.State.Since.Equal(t0) {
		t.Fatalf("%+v", out)
	}
	// The last news goes in the retained state only: the event says the change.
	if out.State.Heard == nil || !out.State.Heard.Equal(heard) || out.Event.Heard != nil || out.Event.Since != nil {
		t.Fatalf("%+v", out)
	}
	schematest.Validate(t, "device-event.json", *out.Event)
	schematest.Validate(t, "device-event.json", *out.State)
	if err := out.State.Validate(); err != nil {
		t.Fatal(err)
	}
	if out := Presence(zigbee.Presence{DeviceID: "d", Since: t0}); out.State.Heard != nil {
		t.Fatalf("silent since the adapter's restart: no last news, %+v", out.State)
	}
	// Online, a device has no last news, even if one was given.
	if out := Presence(zigbee.Presence{DeviceID: "d", Online: true, Since: t0, Heard: heard}); string(out.State.Value) != `"online"` || out.State.Heard != nil {
		t.Fatalf("%+v", out)
	}
}

func TestStatesToClearOnRemoval(t *testing.T) {
	got := StateCapabilities(zigbee.Device{Capabilities: []string{"contact", "battery"}})
	if !reflect.DeepEqual(got, []string{"availability", "battery", "contact"}) {
		t.Fatal(got)
	}
}

func TestAcks(t *testing.T) {
	for in, want := range map[string]string{
		zigbee.AckAccepted: contract.CmdAccepted, zigbee.AckDone: contract.CmdDone,
		zigbee.AckFailed: contract.CmdFailed, zigbee.AckExpired: contract.CmdExpired,
	} {
		a, err := Ack(zigbee.Ack{Ref: "r", Status: in, Reason: "x"})
		if err != nil || a != (contract.CommandAckData{Ref: "r", Status: want, Reason: "x"}) {
			t.Errorf("%s: %+v %v", in, a, err)
		}
		schematest.Validate(t, "device-command-ack.json", a)
	}
	if _, err := Ack(zigbee.Ack{Status: "maybe"}); err == nil {
		t.Fatal("unknown status")
	}
}

func TestOrdersBecomeInputs(t *testing.T) {
	exp := t0.Add(30 * time.Second)
	c := Command(contract.DeviceCommandData{DeviceID: "dev_s", Command: contract.CmdSirenOn, Args: contract.DeviceCommandArgs{DurationS: 300}, Exp: contract.TSPtr(exp)}, "ref", t0)
	if c != (zigbee.Command{Now: t0, Ref: "ref", DeviceID: "dev_s", Name: "siren.on", DurationS: 300, Exp: exp}) {
		t.Fatalf("%+v", c)
	}
	if c := Command(contract.DeviceCommandData{DeviceID: "dev_s", Command: contract.CmdSirenOff}, "r", t0); !c.Exp.IsZero() {
		t.Fatal("no expiry")
	}
	if p := Pair(contract.RegistryPairData{DurationS: 120}, "p", t0); p != (zigbee.Pair{Now: t0, Ref: "p", For: 2 * time.Minute}) {
		t.Fatalf("%+v", p)
	}
	if r := Remove(contract.RegistryRemoveData{DeviceID: "d"}, "r", t0); r != (zigbee.Remove{Now: t0, Ref: "r", DeviceID: "d"}) {
		t.Fatalf("%+v", r)
	}
}

func TestRegistryMessages(t *testing.T) {
	d := zigbee.Device{ID: "dev_k7p2", IEEE: "0x01", Model: "MCCGQ11LM", Profile: "aqara.contact.mccgq11lm", Capabilities: []string{"battery", "contact"}}
	snap := Snapshot([]zigbee.Device{d})
	want := contract.RegistryDevice{DeviceID: "dev_k7p2", Model: "aqara.contact.mccgq11lm", Capabilities: []string{"battery", "contact"}, Adapter: contract.AdapterZigbee}
	if len(snap.Devices) != 1 || !reflect.DeepEqual(snap.Devices[0], want) {
		t.Fatalf("%+v", snap)
	}
	schematest.Validate(t, "registry-snapshot.json", snap)
	if empty := Snapshot(nil); empty.Devices == nil {
		t.Fatal("an empty list, not null")
	}
	schematest.Validate(t, "registry-snapshot.json", Snapshot(nil))

	added := Added(zigbee.Added{Ref: "p", Device: d})
	if added.DeviceID != "dev_k7p2" || added.Model != "aqara.contact.mccgq11lm" || added.Ref != "p" {
		t.Fatalf("%+v", added)
	}
	schematest.Validate(t, "registry-device-added.json", added)

	for in, want := range map[string]string{
		zigbee.RejectUnknownModel: contract.RejectUnknownModel, zigbee.RejectNotPairing: contract.RejectNotPairing,
		zigbee.RejectInterviewFailed: contract.RejectInterviewFailed,
	} {
		r, err := Rejected(zigbee.Rejected{Ref: "p", Model: "X", Reason: in})
		if err != nil || r.Reason != want || r.Model != "X" {
			t.Errorf("%+v %v", r, err)
		}
		schematest.Validate(t, "registry-device-rejected.json", r)
	}
	if _, err := Rejected(zigbee.Rejected{Reason: "?"}); err == nil {
		t.Fatal("unknown reason")
	}
	removed := Removed(zigbee.Removed{Ref: "r", Device: d})
	if removed != (contract.RegistryDeviceRemovedData{Ref: "r", DeviceID: "dev_k7p2"}) {
		t.Fatalf("%+v", removed)
	}
	schematest.Validate(t, "registry-device-removed.json", removed)
}

func TestStatus(t *testing.T) {
	s := Status(zigbee.Status{Bridge: true, Devices: 3, Offline: []string{"d"}, Pairing: true})
	if s.Bridge != contract.BridgeOnline || s.Devices != 3 || s.Offline[0] != "d" || !s.Pairing {
		t.Fatalf("%+v", s)
	}
	schematest.Validate(t, "zigbee-status.json", s)
	s = Status(zigbee.Status{})
	if s.Bridge != contract.BridgeOffline || s.Offline == nil {
		t.Fatalf("%+v", s)
	}
	schematest.Validate(t, "zigbee-status.json", s)
}
