package agent

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"safehome/internal/contract"
)

// An order from the app the agent does not pass on is answered at once: the cloud
// would otherwise wait for an answer that cannot come, and say expired only after a
// minute and more.

func TestAnOrderThatComesTooLateIsAnsweredAtOnce(t *testing.T) {
	h := newHarness(t)
	past := contract.TS(t0)
	order := contract.AlarmCommandData{Command: contract.CmdDisarm, Args: contract.CommandArgs{Code: "482915"},
		Origin: contract.OriginApp, User: "u_owner", Exp: &past}
	id, raw, acts := h.cloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, order)
	if a := of[AnswerOrder](acts); len(a) != 1 || a[0] != (AnswerOrder{Ref: id, Status: contract.AckExpired}) || len(of[PublishLocal](acts)) != 0 {
		t.Fatalf("%+v", acts)
	}
	// Given again by the cloud's broker (QoS 1): answered once.
	if acts := h.handle(Cloud{Now: h.now, Topic: topic("alarm", "cmd"), Payload: raw}); len(acts) != 0 {
		t.Fatalf("answered twice: %+v", acts)
	}
	// Only an order that passed the checks is answered; the others are refused, as
	// before, without a word to the cloud.
	keypad := order
	keypad.Origin = contract.OriginKeypad
	_, _, acts = h.cloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, keypad)
	if len(of[Refused](acts)) != 1 || len(of[AnswerOrder](acts)) != 0 {
		t.Fatalf("an order the app may not send was answered: %+v", acts)
	}
	_, _, acts = h.cloud(contract.Topic("tg-lom-0002", "alarm", "cmd"), contract.TypeAlarmCommand, order)
	if len(of[Refused](acts)) != 1 || len(of[AnswerOrder](acts)) != 0 {
		t.Fatalf("an order for another site was answered: %+v", acts)
	}
}

func TestAnOrderOverTheLimitIsAnsweredAtOnce(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DownBurst = 2; c.DownPerMinute = 6 })
	order := contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp, User: "u_owner"}
	send := func(o contract.AlarmCommandData) (string, []byte, []Action) {
		return h.cloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, o)
	}
	send(order)
	send(order) // the burst
	id, raw, acts := send(order)
	want := AnswerOrder{Ref: id, Status: contract.AckRefused, Reason: contract.ReasonRateLimited}
	if a := of[AnswerOrder](acts); len(a) != 1 || a[0] != want || len(of[PublishLocal](acts)) != 0 {
		t.Fatalf("%+v", acts)
	}
	// Answered for good: given again once the allowance is back, it does not run.
	h.now = h.now.Add(time.Minute)
	if acts := h.handle(Cloud{Now: h.now, Topic: topic("alarm", "cmd"), Payload: raw}); len(acts) != 0 {
		t.Fatalf("an order answered refused reached the hub: %+v", acts)
	}

	send(order)
	send(order) // the allowance again
	keypad := order
	keypad.Origin = contract.OriginKeypad
	if _, _, acts := send(keypad); len(of[Refused](acts)) != 1 || len(of[AnswerOrder](acts)) != 0 {
		t.Fatalf("an order the app may not send was answered: %+v", acts)
	}
	_, _, acts = h.cloud(topic("config", "set"), contract.TypeConfigSet, contract.ConfigSetData{Version: 3})
	if len(of[Refused](acts)) != 1 || len(of[AnswerOrder](acts)) != 0 {
		t.Fatalf("only orders are answered: %+v", acts)
	}
	// The answers have an allowance of their own: a flood gets no more answers than
	// orders would pass, the rest is dropped as before. The orders' allowance is not
	// touched by them.
	answers := 0
	for range 10 {
		_, _, a := send(order)
		answers += len(of[AnswerOrder](a))
	}
	if answers != 2 {
		t.Fatalf("%d answers to a flood, want the burst of 2", answers)
	}
	h.now = h.now.Add(10 * time.Second)
	if _, _, acts := send(order); len(of[PublishLocal](acts)) != 1 {
		t.Fatalf("the orders' allowance did not refill: %+v", acts)
	}
}

// A message refused over the limit changes nothing in the snapshot, for both
// allowances: the driver writes the snapshot (some 60 KB with the ids seen) whenever
// it changes, and a flood from a broken cloud must not become a flood of writes to
// the SD card.
func TestAFloodOverTheLimitWritesNothing(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DownBurst = 2; c.DownPerMinute = 6 })
	order := contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp, User: "u_owner"}
	for range 4 { // the orders' burst, then the answers' burst
		h.cloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, order)
	}
	before, _ := json.Marshal(h.r.Snapshot())
	for range 50 {
		h.now = h.now.Add(100 * time.Millisecond) // a tenth of a message in all: still under one
		_, _, a := h.cloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, order)
		_, _, b := h.cloud(topic("config", "set"), contract.TypeConfigSet, contract.ConfigSetData{Version: 3})
		if len(of[Refused](a)) != 1 || len(of[Refused](b)) != 1 {
			t.Fatalf("not refused: %+v %+v", a, b)
		}
	}
	if after, _ := json.Marshal(h.r.Snapshot()); !bytes.Equal(before, after) {
		t.Fatalf("a refused message changed the snapshot:\n%s\n%s", before, after)
	}
}

// A large alarm: the cloud's answers to the alerts and the app's acknowledgements
// come by the dozen. They are not orders, the limit does not drop them.
func TestRepliesDoNotCountAgainstTheLimit(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DownBurst = 2; c.DownPerMinute = 6 })
	h.linkUp()
	h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered) // incident inc1 goes up
	id, _ := h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
	it, _ := h.next()
	h.handle(Sent{Now: h.now, ID: it.ID})
	for _, status := range []string{contract.ResultQueued, contract.ResultSent, contract.ResultDelivered} {
		_, _, acts := h.cloud(topic("alert", "result"), contract.TypeAlertResult, contract.AlertResultData{Ref: id, Status: status})
		if len(of[PublishLocal](acts)) != 1 {
			t.Fatalf("%s: %+v", status, acts)
		}
	}
	_, _, acts := h.cloud(topic("alert", "ack"), contract.TypeAlertAck, contract.AlertAckData{Incident: "inc1", Contact: "u_owner", Via: contract.ChannelApp})
	if len(of[PublishLocal](acts)) != 1 {
		t.Fatalf("the app's ack: %+v", acts)
	}
	// The orders still have their allowance, and only that.
	order := contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp, User: "u_owner"}
	for i := range 3 {
		_, _, acts := h.cloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, order)
		if limited := len(of[AnswerOrder](acts)) == 1; limited != (i == 2) {
			t.Fatalf("order %d: %+v", i, acts)
		}
	}
}
