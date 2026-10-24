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
	if len(out) != 1 || out[0] != (PermitJoin{Seconds: 120, Txn: "p1.1"}) || h.n.Status().Pairing {
		t.Fatalf("not open before Zigbee2MQTT says so: %+v", out)
	}
	none(t, h.do(JoinAnswer{Now: h.now, Txn: "p1.1", OK: true}))
	if !h.n.Status().Pairing {
		t.Fatal("open once Zigbee2MQTT confirmed")
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

// The Heiman siren of the catalogue reports nothing when it sounds: it only takes
// siren.on and siren.off. It is still a siren, or no installer could make it one.
func TestADeviceDrivenAsASirenIsASiren(t *testing.T) {
	quiet := sirenProfile()
	quiet.Capabilities = map[string]Rule{"link": {From: "linkquality", Max: 255}}
	for name, cmd := range quiet.Commands {
		cmd.Confirm = nil
		quiet.Commands[name] = cmd
	}
	h := newHarness(t, func(c *Config) { c.Profiles = []Profile{quiet} })
	h.ids = []string{"dev_s1"}
	h.do(Pair{Now: h.now, Ref: "p1", For: time.Minute})
	added, ok := find[Added](h.do(Interviewed{Now: h.now, IEEE: "0x04", Model: "HS2WD-E", OK: true}))
	if !ok || !reflect.DeepEqual(added.Device.Capabilities, []string{"link", "siren"}) {
		t.Fatalf("%+v", added)
	}
}

func TestPairingIsBoundedAndCanBeClosed(t *testing.T) {
	h := newHarness(t)
	if out := h.do(Pair{Now: h.now, Ref: "p", For: time.Hour}); out[0] != (PermitJoin{Seconds: 254, Txn: "p.1"}) {
		t.Fatalf("%+v", out)
	}
	if !h.n.Snapshot().PairUntil.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("bounded to PairMax: %v", h.n.Snapshot().PairUntil)
	}
	if out := h.do(Pair{Now: h.now, Ref: "p"}); len(out) != 1 || out[0] != (PermitJoin{}) || h.n.Status().Pairing {
		t.Fatalf("%+v", out)
	}
	none(t, h.advance(time.Hour))
}

// The trial on real hardware: -for 5m asked Zigbee2MQTT for 300 s at once, which it
// refuses (254 s at most), and the network never opened. A window longer than one
// request is opened in pieces, each asked before the previous one ends.
func TestALongWindowIsOpenedInPieces(t *testing.T) {
	for _, tc := range []struct {
		For    time.Duration
		pieces []int // seconds asked, in order
		at     []time.Duration
	}{
		{For: 5 * time.Minute, pieces: []int{254, 56}, at: []time.Duration{0, 244 * time.Second}},
		{For: 10 * time.Minute, pieces: []int{254, 254, 112}, at: []time.Duration{0, 244 * time.Second, 488 * time.Second}},
		{For: 254 * time.Second, pieces: []int{254}, at: []time.Duration{0}},
	} {
		t.Run(tc.For.String(), func(t *testing.T) {
			h := newHarness(t)
			first := h.do(Pair{Now: h.now, Ref: "p", For: tc.For})[0].(PermitJoin)
			h.do(JoinAnswer{Now: h.now, Txn: first.Txn, OK: true})
			got := []int{first.Seconds}
			at := []time.Duration{0}
			for {
				wake := h.n.NextWake()
				if wake.IsZero() {
					t.Fatal("the window must close by itself")
				}
				out := h.advance(wake.Sub(h.now))
				pj, ok := find[PermitJoin](out)
				if !ok {
					t.Fatalf("woken for nothing: %+v", out)
				}
				if pj.Seconds == 0 {
					break
				}
				h.do(JoinAnswer{Now: h.now, Txn: pj.Txn, OK: true})
				got = append(got, pj.Seconds)
				at = append(at, h.now.Sub(t0))
				if !h.n.Status().Pairing {
					t.Fatal("open all along")
				}
			}
			if !reflect.DeepEqual(got, tc.pieces) || !reflect.DeepEqual(at, tc.at) {
				t.Fatalf("asked %v at %v, want %v at %v", got, at, tc.pieces, tc.at)
			}
			if h.now.Sub(t0) != tc.For || h.n.Status().Pairing {
				t.Fatalf("closed at %v", h.now.Sub(t0))
			}
		})
	}
}

func TestAWindowGoesOnAfterARestart(t *testing.T) {
	h := newHarness(t)
	h.do(Pair{Now: h.now, Ref: "p", For: 5 * time.Minute})
	h.do(JoinAnswer{Now: h.now, Txn: "p.1", OK: true})
	snap := h.n.Snapshot()
	// The adapter closed the network as it stopped: the next input opens it again,
	// for what is left of the window.
	h.n.Restore(snap)
	pj, ok := find[PermitJoin](h.advance(30 * time.Second))
	if !ok || pj.Seconds != 254 || pj.Txn != "p.2" {
		t.Fatalf("%+v", pj)
	}
	if !h.n.Status().Pairing {
		t.Fatal("the window goes on while the network opens again")
	}
	h.do(JoinAnswer{Now: h.now, Txn: pj.Txn, OK: true})
	if !h.n.NextWake().Equal(h.now.Add(244 * time.Second)) {
		t.Fatalf("next piece at %v", h.n.NextWake())
	}
	// Down past the renewal: the rest at once.
	h.n.Restore(h.n.Snapshot())
	if pj, ok := find[PermitJoin](h.advance(220 * time.Second)); !ok || pj.Seconds != 50 {
		t.Fatalf("%+v", pj)
	} else {
		h.do(JoinAnswer{Now: h.now, Txn: pj.Txn, OK: true})
	}
	// Down past the end: closed for good.
	h.n.Restore(h.n.Snapshot())
	if out := h.advance(time.Hour); len(out) != 1 || out[0] != (PermitJoin{}) || h.n.Status().Pairing {
		t.Fatalf("%+v", out)
	}
}

func TestARefusedOpeningEndsTheWindow(t *testing.T) {
	h := newHarness(t)
	h.do(Pair{Now: h.now, Ref: "p1", For: 5 * time.Minute})
	out := h.do(JoinAnswer{Now: h.now, Txn: "p1.1", Reason: "Cannot permit join for more than 254 seconds."})
	want := []Action{PermitJoin{}, PairFailed{Ref: "p1", Reason: "Cannot permit join for more than 254 seconds."}}
	if !reflect.DeepEqual(out, want) || h.n.Status().Pairing {
		t.Fatalf("%+v", out)
	}
	none(t, h.advance(time.Hour)) // no piece asked any more
	if _, ok := find[Ignored](h.do(JoinAnswer{Now: h.now, Txn: "p1.1", Reason: "x"})); !ok {
		t.Fatal("a refusal while closed changes nothing")
	}
}

// A refusal is matched to its request by the transaction Zigbee2MQTT echoes: a late
// refusal of a closing order, or of a window replaced since, must not end the window
// open now.
func TestOnlyTheAnswerToTheRequestAwaitedCounts(t *testing.T) {
	h := newHarness(t)
	h.do(Pair{Now: h.now, Ref: "p1", For: 2 * time.Minute})
	h.do(JoinAnswer{Now: h.now, Txn: "p1.1", OK: true})
	h.do(Pair{Now: h.now, Ref: "p1"}) // closed, Zigbee2MQTT has not answered yet
	if out := h.do(Pair{Now: h.now, Ref: "p2", For: 2 * time.Minute}); out[0] != (PermitJoin{Seconds: 120, Txn: "p2.1"}) {
		t.Fatalf("%+v", out)
	}
	h.do(JoinAnswer{Now: h.now, Txn: "p2.1", OK: true})
	for _, late := range []JoinAnswer{
		{Now: h.now, Reason: "Coordinator is busy"},                    // the closing order: no transaction
		{Now: h.now, Txn: "p1.1", Reason: "Coordinator is busy"},       // the window before
		{Now: h.now, Txn: "p2.1", Reason: "answered twice"},            // already answered
		{Now: h.now, Txn: "someone else's", Reason: "not our request"}, // another client
	} {
		if _, ok := find[Ignored](h.do(late)); !ok || !h.n.Status().Pairing {
			t.Fatalf("%+v ended the window", late)
		}
	}
	// A new order while the network is open is a new window, open once confirmed.
	if out := h.do(Pair{Now: h.now, Ref: "p3", For: time.Minute}); out[0] != (PermitJoin{Seconds: 60, Txn: "p3.1"}) || h.n.Status().Pairing {
		t.Fatalf("%+v", out)
	}
	if _, ok := find[PairFailed](h.do(JoinAnswer{Now: h.now, Txn: "p3.1", Reason: "refused"})); !ok {
		t.Fatal("the refusal of the request awaited ends the window")
	}
}

// Zigbee2MQTT down or stuck answers nothing: the installer's tool trusted the
// adapter's own word that the network was open. The request is sent again, as a
// device command is, then the window fails.
func TestAWindowZigbee2MQTTNeverConfirmsFails(t *testing.T) {
	h := newHarness(t)
	h.do(Pair{Now: h.now, Ref: "p1", For: 2 * time.Minute})
	if !h.n.NextWake().Equal(t0.Add(3 * time.Second)) {
		t.Fatalf("next wake %v", h.n.NextWake())
	}
	for i, wantSeconds := range []int{117, 114} {
		out := h.advance(3 * time.Second)
		if len(out) != 1 || out[0] != (PermitJoin{Seconds: wantSeconds, Txn: "p1.1"}) || h.n.Status().Pairing {
			t.Fatalf("attempt %d: %+v", i+2, out)
		}
	}
	out := h.advance(3 * time.Second)
	want := []Action{PermitJoin{}, PairFailed{Ref: "p1", Reason: ReasonNoJoinAnswer}}
	if !reflect.DeepEqual(out, want) || h.n.Status().Pairing || !h.n.NextWake().IsZero() {
		t.Fatalf("%+v", out)
	}

	// A late answer to the first request, sent again under the same transaction,
	// still opens the window.
	h.do(Pair{Now: h.now, Ref: "p2", For: 2 * time.Minute})
	h.advance(3 * time.Second)
	if out := h.do(JoinAnswer{Now: h.now, Txn: "p2.1", OK: true}); len(out) != 0 || !h.n.Status().Pairing {
		t.Fatalf("%+v", out)
	}
	none(t, h.advance(10*time.Second)) // nothing asked again
}

// A piece asked to keep the window open is awaited too; the window stays open
// meanwhile (the previous piece runs), and fails if Zigbee2MQTT stops answering.
func TestARenewalZigbee2MQTTNeverConfirmsEndsTheWindow(t *testing.T) {
	h := newHarness(t)
	h.do(Pair{Now: h.now, Ref: "p", For: 5 * time.Minute})
	h.do(JoinAnswer{Now: h.now, Txn: "p.1", OK: true})
	pj, _ := find[PermitJoin](h.advance(244 * time.Second))
	if pj.Txn != "p.2" || !h.n.Status().Pairing {
		t.Fatalf("%+v", pj)
	}
	h.advance(3 * time.Second)
	h.advance(3 * time.Second)
	if f, ok := find[PairFailed](h.advance(3 * time.Second)); !ok || f.Ref != "p" || h.n.Status().Pairing {
		t.Fatalf("%+v", f)
	}
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
