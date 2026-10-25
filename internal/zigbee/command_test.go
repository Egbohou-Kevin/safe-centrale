package zigbee

import (
	"reflect"
	"testing"
	"time"
)

// --- commands ------------------------------------------------------------------

func TestACommandIsSentAndConfirmedByTheDevice(t *testing.T) {
	h := newHarness(t)
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	out := h.do(Command{Now: h.now, Ref: "c1", DeviceID: "dev_siren", Name: "siren.on", DurationS: 300, Exp: h.now.Add(30 * time.Second)})
	send, ok := find[Send](out)
	want := map[string]any{"warning": map[string]any{"mode": "burglar", "duration": 300, "steps": []any{300, 1}}}
	if !ok || send.IEEE != ieeeSiren || !reflect.DeepEqual(send.Payload, want) {
		t.Fatalf("%+v", out)
	}
	if acks := all[Ack](out); len(acks) != 1 || acks[0] != (Ack{Ref: "c1", DeviceID: "dev_siren", Status: AckAccepted}) {
		t.Fatalf("%+v", out)
	}
	if !h.n.NextWake().Equal(t0.Add(3 * time.Second)) {
		t.Fatalf("next wake %v", h.n.NextWake())
	}
	out = h.report("dev_siren", map[string]any{"alarm": "ON"})
	if acks := all[Ack](out); len(acks) != 1 || acks[0].Status != AckDone || acks[0].Ref != "c1" {
		t.Fatalf("%+v", out)
	}
	// Already on: a second siren.on is confirmed by the next report even without change.
	h.do(Command{Now: h.now, Ref: "c2", DeviceID: "dev_siren", Name: "siren.on", DurationS: 60})
	if acks := all[Ack](h.report("dev_siren", map[string]any{"alarm": "ON"})); len(acks) != 1 || acks[0].Ref != "c2" {
		t.Fatal("the state it waits for, changed or not")
	}
	// A retained message is no confirmation.
	h.do(Command{Now: h.now, Ref: "c3", DeviceID: "dev_siren", Name: "siren.off"})
	if _, ok := find[Ack](h.retained("dev_siren", map[string]any{"alarm": "OFF"})); ok {
		t.Fatal("an old state confirms nothing")
	}
	if _, ok := find[Ack](h.report("dev_siren", map[string]any{"linkquality": 3.0})); ok {
		t.Fatal("a message without the field confirms nothing")
	}
}

func TestAnUnconfirmedCommandIsSentAgainThenFails(t *testing.T) {
	h := newHarness(t)
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	h.do(Command{Now: h.now, Ref: "c1", DeviceID: "dev_siren", Name: "siren.off"})
	for i := range 2 {
		out := h.advance(3 * time.Second)
		if _, ok := find[Send](out); !ok {
			t.Fatalf("attempt %d: %+v", i+2, out)
		}
	}
	out := h.advance(3 * time.Second)
	if acks := all[Ack](out); len(acks) != 1 || acks[0].Status != AckFailed || acks[0].Reason != ReasonNoConfirmation {
		t.Fatalf("%+v", out)
	}
	if _, ok := find[Send](out); ok {
		t.Fatal("three attempts in all")
	}
	none(t, h.advance(time.Minute))
}

func TestACommandPastItsExpiryIsNotCarriedOut(t *testing.T) {
	h := newHarness(t)
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	out := h.do(Command{Now: h.now, Ref: "c1", DeviceID: "dev_siren", Name: "siren.on", DurationS: 60, Exp: h.now})
	if len(out) != 1 || out[0] != (Ack{Ref: "c1", DeviceID: "dev_siren", Status: AckExpired}) {
		t.Fatalf("%+v", out)
	}
	// Expiry reached while waiting for the confirmation.
	h.do(Command{Now: h.now, Ref: "c2", DeviceID: "dev_siren", Name: "siren.on", DurationS: 60, Exp: h.now.Add(5 * time.Second)})
	h.advance(3 * time.Second) // sent again
	out = h.advance(3 * time.Second)
	if acks := all[Ack](out); len(acks) != 1 || acks[0].Status != AckExpired || acks[0].Ref != "c2" {
		t.Fatalf("%+v", out)
	}
}

func TestCommandsThatCannotBeCarriedOut(t *testing.T) {
	h := newHarness(t)
	h.add("dev_door", ieeeDoor, "MCCGQ11LM")
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	if out := h.do(Command{Now: h.now, Ref: "c1", DeviceID: "dev_nope", Name: "siren.on"}); len(out) != 1 || out[0].(Ack).Reason != ReasonUnknownDevice {
		t.Fatalf("%+v", out)
	}
	if out := h.do(Command{Now: h.now, Ref: "c2", DeviceID: "dev_door", Name: "siren.on"}); len(out) != 1 || out[0].(Ack).Reason != ReasonUnsupported {
		t.Fatalf("%+v", out)
	}
	// Without a confirmation in the profile, a command is done once sent.
	out := h.do(Command{Now: h.now, Ref: "c3", DeviceID: "dev_siren", Name: "device.identify"})
	if acks := all[Ack](out); len(acks) != 2 || acks[0].Status != AckAccepted || acks[1].Status != AckDone {
		t.Fatalf("%+v", out)
	}
}

func TestANewCommandSupersedesTheOneWaiting(t *testing.T) {
	h := newHarness(t)
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	h.do(Command{Now: h.now, Ref: "on", DeviceID: "dev_siren", Name: "siren.on", DurationS: 60})
	out := h.do(Command{Now: h.now, Ref: "off", DeviceID: "dev_siren", Name: "siren.off"})
	acks := all[Ack](out)
	if len(acks) != 2 || acks[0] != (Ack{Ref: "on", DeviceID: "dev_siren", Status: AckFailed, Reason: ReasonSuperseded}) || acks[1].Ref != "off" {
		t.Fatalf("%+v", out)
	}
	if h.n.Snapshot().Pending["dev_siren"].Ref != "off" {
		t.Fatal("one command at a time per device")
	}
}

func TestOnlyTheValueReadFromTheMessageConfirms(t *testing.T) {
	h := newHarness(t)
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	h.report("dev_siren", map[string]any{"alarm": "ON"}) // the state kept is already "on"
	h.do(Command{Now: h.now, Ref: "c1", DeviceID: "dev_siren", Name: "siren.on", DurationS: 60})
	if _, ok := find[Ack](h.report("dev_siren", map[string]any{"alarm": "garbled"})); ok {
		t.Fatal("a field that cannot be read confirms nothing, whatever the state kept")
	}
	if acks := all[Ack](h.report("dev_siren", map[string]any{"alarm": "ON"})); len(acks) != 1 || acks[0].Status != AckDone {
		t.Fatal("the value it waits for confirms")
	}
}

func TestAWaitingCommandItsProfileLostFailsInsteadOfSendingNothing(t *testing.T) {
	h := newHarness(t)
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	h.do(Command{Now: h.now, Ref: "c1", DeviceID: "dev_siren", Name: "siren.on", DurationS: 60})
	// Restarted with a profile that no longer has the command.
	again := newHarness(t, func(c *Config) { delete(c.Profiles[1].Commands, "siren.on") })
	again.n.Restore(h.n.Snapshot())
	again.now = h.now
	out := again.advance(3 * time.Second)
	if _, ok := find[Send](out); ok {
		t.Fatalf("an empty payload sent: %+v", out)
	}
	if acks := all[Ack](out); len(acks) != 1 || acks[0] != (Ack{Ref: "c1", DeviceID: "dev_siren", Status: AckFailed, Reason: ReasonUnsupported}) {
		t.Fatalf("%+v", out)
	}
}

// The clock is set back while a command waits for its confirmation, and a pairing
// window runs: the siren is sent again 3 s later, not an hour, and the window stays
// open no longer than the longest one.
func TestAClockSetBackStretchesNoWait(t *testing.T) {
	h := newHarness(t)
	h.add("dev_siren", ieeeSiren, "HS2WD-E")
	h.do(Command{Now: h.now, Ref: "c1", DeviceID: "dev_siren", Name: "siren.off"})
	h.do(Pair{Now: h.now, Ref: "pair-2", For: 2 * time.Minute})
	h.now = h.now.Add(-time.Hour)
	h.do(Tick{Now: h.now})
	if _, ok := find[Send](h.advance(3 * time.Second)); !ok {
		t.Fatal("the siren is sent again after its wait")
	}
	if !h.n.Snapshot().PairUntil.Equal(h.now.Add(-3*time.Second + h.n.cfg.PairMax)) {
		t.Fatalf("the window ends at most its length after the step: %v", h.n.Snapshot().PairUntil)
	}
}
