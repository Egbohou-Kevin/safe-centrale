package zigbee

import (
	"reflect"
	"testing"
	"time"
)

// --- devices questioned after the window, strangers, Zigbee2MQTT restarting ------

const ieeeStranger = "0x00158d00010000aa"

// openWindow opens a confirmed pairing window of d under ref p1.
func (h *harness) openWindow(d time.Duration) {
	h.t.Helper()
	h.do(Pair{Now: h.now, Ref: "p1", For: d})
	h.do(JoinAnswer{Now: h.now, Txn: "p1.1", OK: true})
}

// A door that enters 100 s into a 120 s window finishes its questioning around 130 s:
// it entered during the window, it is kept under the window's order. Until then the
// network says it is pairing, for the installer's tool to wait for it.
func TestAnInterviewThatEndsAfterTheWindowIsDecidedByTheWindowItEntered(t *testing.T) {
	h := newHarness(t)
	h.ids = []string{"dev_door"}
	h.openWindow(2 * time.Minute)
	h.now = t0.Add(100 * time.Second)
	none(t, h.do(Joined{Now: h.now, IEEE: ieeeDoor}))
	if out := h.advance(20 * time.Second); len(out) != 1 || out[0] != (PermitJoin{}) {
		t.Fatalf("the network closes at the end of the window: %+v", out)
	}
	if !h.n.Status().Pairing {
		t.Fatal("a device being questioned: the pairing is not over")
	}
	h = h.restarted(t0.Add(130 * time.Second)) // and it survives a restart
	h.ids = []string{"dev_door"}
	out := h.do(Interviewed{Now: h.now, IEEE: ieeeDoor, Model: "MCCGQ11LM", OK: true})
	if a, ok := find[Added](out); !ok || a.Ref != "p1" || a.Device.ID != "dev_door" {
		t.Fatalf("kept under the window's order: %+v", out)
	}
	if h.n.Status().Pairing || len(h.n.Snapshot().Joining) != 0 {
		t.Fatal("nothing left to wait for")
	}
}

func TestADeviceQuestionedAfterTheWindowIsRefusedUnderItsOrder(t *testing.T) {
	h := newHarness(t)
	h.openWindow(time.Minute)
	h.do(Joined{Now: h.now, IEEE: ieeeDoor})
	h.do(Joined{Now: h.now, IEEE: ieeeSiren})
	h.advance(90 * time.Second)
	out := h.do(Interviewed{Now: h.now, IEEE: ieeeDoor})
	if r, ok := find[Rejected](out); !ok || r != (Rejected{Ref: "p1", Reason: RejectInterviewFailed}) {
		t.Fatalf("%+v", out)
	}
	// The other one is never decided: it is forgotten in time, and the network stops
	// saying it is pairing.
	if want := t0.Add(JoinWait); !h.n.NextWake().Equal(want) {
		t.Fatalf("next wake %v, want %v", h.n.NextWake(), want)
	}
	h.advance(JoinWait)
	if h.n.Status().Pairing || len(h.n.Snapshot().Joining) != 0 {
		t.Fatal("the questioning took too long")
	}
	if r, ok := find[Rejected](h.do(Interviewed{Now: h.now, IEEE: ieeeSiren, Model: "HS2WD-E", OK: true})); !ok || r.Reason != RejectNotPairing || r.Ref != "" {
		t.Fatalf("too late: %+v", r)
	}
}

func TestADeviceEnteringOutsideAWindowIsStillRefused(t *testing.T) {
	h := newHarness(t)
	none(t, h.do(Joined{Now: h.now, IEEE: ieeeDoor}))
	if h.n.Status().Pairing {
		t.Fatal("no window")
	}
	out := h.do(Interviewed{Now: h.now, IEEE: ieeeDoor, Model: "MCCGQ11LM", OK: true})
	if r, ok := find[Rejected](out); !ok || r.Reason != RejectNotPairing {
		t.Fatalf("%+v", out)
	}
	// A registered device joining again is no new device.
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	h.openWindow(time.Minute)
	h.do(Joined{Now: h.now, IEEE: ieeeDoor})
	if len(h.n.Snapshot().Joining) != 0 {
		t.Fatal("a registered device is not questioned for pairing")
	}
}

// Zigbee2MQTT starts again with the network closed: the window ends, and says so, for
// the installer not to wait in front of a closed network. Coming back opens nothing.
func TestZigbee2MQTTGoingOfflineEndsTheWindow(t *testing.T) {
	h := newHarness(t)
	h.do(Bridge{Now: h.now, Online: true})
	h.openWindow(2 * time.Minute)
	h.now = h.now.Add(10 * time.Second)
	out := h.do(Bridge{Now: h.now})
	want := []Action{PermitJoin{}, PairFailed{Ref: "p1", Reason: ReasonBridgeOffline}}
	if !reflect.DeepEqual(out, want) || h.n.Status().Pairing {
		t.Fatalf("%+v", out)
	}
	h.now = h.now.Add(10 * time.Second)
	if _, ok := find[PermitJoin](h.do(Bridge{Now: h.now, Online: true})); ok {
		t.Fatal("no window any more")
	}
	none(t, h.do(Bridge{Now: h.now}))
}

// A removed door asleep does not answer the request to leave: it stays in Zigbee2MQTT's
// network under its old name. When it speaks, it is asked again while awake, then
// removed by force, and said in the journal; then left alone until the next start.
func TestARemovedDeviceThatStillSpeaksIsAskedToLeave(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	h.do(Remove{Now: h.now, Ref: "r1", DeviceID: "dev_door"})
	for _, want := range [][]Action{
		{Expel{IEEE: "dev_door"}, Stray{Name: "dev_door", Removed: true}},
		{Expel{IEEE: "dev_door", Force: true}, Stray{Name: "dev_door", Removed: true, Forced: true}},
	} {
		if out := h.report("dev_door", map[string]any{"contact": true}); !reflect.DeepEqual(out, want) {
			t.Fatalf("got %+v, want %+v", out, want)
		}
	}
	if _, ok := find[Ignored](h.report("dev_door", map[string]any{"contact": true})); !ok {
		t.Fatal("asked twice is enough")
	}
	h = h.restarted(h.now)
	if _, ok := find[Expel](h.report("dev_door", map[string]any{"contact": true})); !ok {
		t.Fatal("asked again after a start")
	}
}

// A device that entered while the adapter was down (or whose questioning ended then)
// is in the network under its IEEE address, outside the registry: it is refused and
// asked to leave when it speaks. Not while a window is open or it is being questioned.
func TestADeviceOutsideTheRegistryIsRefusedWhenItSpeaks(t *testing.T) {
	h := newHarness(t)
	out := h.report(ieeeStranger, map[string]any{"contact": true})
	want := []Action{Expel{IEEE: ieeeStranger}, Stray{Name: ieeeStranger}, Rejected{Reason: RejectNotPairing}}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("got %+v", out)
	}
	if _, ok := find[Rejected](h.report(ieeeStranger, nil)); ok {
		t.Fatal("refused once")
	}
	for name, h := range map[string]*harness{"window": newHarness(t), "questioned": newHarness(t)} {
		h.openWindow(time.Minute)
		h.do(Joined{Now: h.now, IEEE: ieeeStranger})
		if name == "questioned" {
			h.advance(time.Minute)
		}
		if _, ok := find[Ignored](h.report(ieeeStranger, map[string]any{"contact": true})); !ok {
			t.Fatalf("%s: not decided yet", name)
		}
	}
	for _, name := range []string{"living_room", "0x1234"} {
		if _, ok := find[Ignored](h.report(name, nil)); !ok {
			t.Fatalf("%s is not a device of ours", name)
		}
	}
	if _, ok := find[Ignored](h.retained("0x00158d00010000bb", nil)); !ok {
		t.Fatal("a retained message is old news")
	}
}
