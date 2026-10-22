package agent

import (
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
