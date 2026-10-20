package zigbee

import (
	"reflect"
	"testing"
	"time"
)

// --- pairing and removal -------------------------------------------------------

func TestPairingAcceptsAModelWithAProfile(t *testing.T) {
	h := newHarness(t)
	h.ids = []string{"dev_k7p2"}
	out := h.do(Pair{Now: h.now, Ref: "p1", For: 2 * time.Minute})
	if len(out) != 1 || out[0] != (PermitJoin{Seconds: 120}) || !h.n.Status().Pairing {
		t.Fatalf("%+v", out)
	}
	out = h.do(Interviewed{Now: h.now, IEEE: ieeeDoor, Model: "MCCGQ11LM", OK: true})
	want := Device{ID: "dev_k7p2", IEEE: ieeeDoor, Model: "MCCGQ11LM", Profile: "aqara.contact.mccgq11lm",
		Capabilities: []string{"battery", "contact", "link", "tamper"}}
	if len(out) != 3 || out[0] != (Rename{IEEE: ieeeDoor, To: "dev_k7p2"}) ||
		!reflect.DeepEqual(out[1], Added{Ref: "p1", Device: want}) ||
		!reflect.DeepEqual(out[2], RegistryChanged{Devices: []Device{want}}) {
		t.Fatalf("%+v", out)
	}
	if _, ok := find[Ignored](h.do(Interviewed{Now: h.now, IEEE: ieeeDoor, Model: "MCCGQ11LM", OK: true})); !ok {
		t.Fatal("questioned again: nothing changes")
	}
	// The window closes by itself.
	if !h.n.NextWake().Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("next wake %v", h.n.NextWake())
	}
	if out := h.advance(2 * time.Minute); len(out) != 1 || out[0] != (PermitJoin{}) || h.n.Status().Pairing {
		t.Fatalf("%+v", out)
	}
}

func TestPairingIsBoundedAndCanBeClosed(t *testing.T) {
	h := newHarness(t)
	if out := h.do(Pair{Now: h.now, Ref: "p", For: time.Hour}); out[0] != (PermitJoin{Seconds: 600}) {
		t.Fatalf("%+v", out)
	}
	if out := h.do(Pair{Now: h.now, Ref: "p"}); len(out) != 1 || out[0] != (PermitJoin{}) || h.n.Status().Pairing {
		t.Fatalf("%+v", out)
	}
	none(t, h.advance(time.Hour))
}

func TestDevicesWeCannotWatchAreRefused(t *testing.T) {
	h := newHarness(t)
	out := h.do(Interviewed{Now: h.now, IEEE: "0x01", Model: "MCCGQ11LM", OK: true})
	if !reflect.DeepEqual(out, []Action{Expel{IEEE: "0x01"}, Rejected{Model: "MCCGQ11LM", Reason: RejectNotPairing}}) {
		t.Fatalf("outside a pairing window: %+v", out)
	}
	h.do(Pair{Now: h.now, Ref: "p1", For: time.Minute})
	out = h.do(Interviewed{Now: h.now, IEEE: "0x02", Model: "WXKG01LM", OK: true})
	if !reflect.DeepEqual(out, []Action{Expel{IEEE: "0x02"}, Rejected{Ref: "p1", Model: "WXKG01LM", Reason: RejectUnknownModel}}) {
		t.Fatalf("no profile: %+v", out)
	}
	out = h.do(Interviewed{Now: h.now, IEEE: "0x03", OK: false})
	if !reflect.DeepEqual(out, []Action{Expel{IEEE: "0x03"}, Rejected{Ref: "p1", Reason: RejectInterviewFailed}}) {
		t.Fatalf("model unknown: %+v", out)
	}
	if h.n.Status().Devices != 0 {
		t.Fatal("nothing registered")
	}
}

func TestDeviceIdsAreNeverGivenTwice(t *testing.T) {
	h := newHarness(t)
	h.add("dev_a", ieeeDoor, "MCCGQ11LM")
	h.do(Remove{Now: h.now, Ref: "r", DeviceID: "dev_a"})
	h.ids = []string{"dev_a", "", "dev_b"} // retired, empty, then free
	h.do(Pair{Now: h.now, Ref: "p", For: time.Minute})
	out := h.do(Interviewed{Now: h.now, IEEE: ieeeSiren, Model: "HS2WD-E", OK: true})
	if a, _ := find[Added](out); a.Device.ID != "dev_b" {
		t.Fatalf("%+v", out)
	}
	// An id generator that only repeats itself: the device is refused rather than
	// given a taken id.
	h.ids = nil
	stuck := newHarness(t)
	stuck.add("dev_x", ieeeDoor, "MCCGQ11LM")
	stuck.n.newID = func() string { return "dev_x" }
	stuck.do(Pair{Now: stuck.now, Ref: "p", For: time.Minute})
	out = stuck.do(Interviewed{Now: stuck.now, IEEE: ieeeSiren, Model: "HS2WD-E", OK: true})
	if r, ok := find[Rejected](out); !ok || r.Reason != RejectInterviewFailed {
		t.Fatalf("%+v", out)
	}
}

func TestRemovalForgetsTheDevice(t *testing.T) {
	h := newHarness(t)
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	h.report("dev_siren", map[string]any{"alarm": "OFF"})
	h.do(Command{Now: h.now, Ref: "c1", DeviceID: "dev_siren", Name: "siren.on", DurationS: 60})
	out := h.do(Remove{Now: h.now, Ref: "r1", DeviceID: "dev_siren"})
	if len(out) != 4 || out[0] != (Expel{IEEE: ieeeSiren}) ||
		out[1] != (Ack{Ref: "c1", DeviceID: "dev_siren", Status: AckFailed, Reason: ReasonRemoved}) {
		t.Fatalf("%+v", out)
	}
	if r := out[2].(Removed); r.Ref != "r1" || r.Device.ID != "dev_siren" || out[3].(RegistryChanged).Devices == nil || len(out[3].(RegistryChanged).Devices) != 0 {
		t.Fatalf("%+v", out)
	}
	s := h.n.Snapshot()
	if len(s.Devices) != 0 || len(s.States) != 0 || len(s.Pending) != 0 || !s.Retired["dev_siren"] {
		t.Fatalf("%+v", s)
	}
	if _, ok := find[Ignored](h.do(Remove{Now: h.now, Ref: "r2", DeviceID: "dev_siren"})); !ok {
		t.Fatal("removed twice")
	}
	none(t, h.advance(5*time.Hour)) // no presence watch, no command left
}
