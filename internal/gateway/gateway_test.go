package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/gsm"
)

func TestTheStatusIsRepeatedEvenWhenNothingChanged(t *testing.T) {
	const every = 47 * time.Second // no other timer of the gateway uses it
	r := newRig(t, nil, func(c *Config) { c.GSM.CheckEvery = time.Hour; c.StatusEvery = every })
	before := len(r.broker.ofType(contract.TypeGSMStatus))
	stamp := func() time.Time {
		all := r.broker.ofType(contract.TypeGSMStatus)
		return all[len(all)-1].env.TS.Time
	}
	r.waitTimer(t0.Add(every))
	r.clock.Advance(every)
	r.waitFor("the repeated status", func() bool { return len(r.broker.ofType(contract.TypeGSMStatus)) == before+1 })
	if !stamp().Equal(t0.Add(every)) {
		t.Fatalf("stamped %v: the hub judges the gateway alive by this time", stamp())
	}
	r.waitTimer(t0.Add(2 * every))
	r.clock.Advance(every)
	r.waitFor("once more", func() bool { return len(r.broker.ofType(contract.TypeGSMStatus)) == before+2 })
}

func TestTheLinesStatusIsPublishedRetainedAtStart(t *testing.T) {
	r := newRig(t, nil)
	st := r.status()
	if st.Lines[0].Operator != "TOGOCOM" || !st.Lines[0].Responding || st.Lines[0].Signal != 70 || st.Queued != 0 {
		t.Fatalf("%+v", st)
	}
	all := r.broker.ofType(contract.TypeGSMStatus)
	if !all[len(all)-1].retain || all[len(all)-1].topic != contract.GSMStatusTopic(site) {
		t.Fatal("the status is retained, on its topic")
	}
	r.validateAll()
}

func TestAnSMSRequestIsSentAndReported(t *testing.T) {
	r := newRig(t, nil)
	ref := r.request(contract.ChannelSMS, "u_owner", contract.AlertIntrusion)
	r.waitFor("queued then sent", func() bool { return len(r.results(ref)) == 2 })
	if got := r.results(ref); got[0] != "queued/" || got[1] != "sent/" {
		t.Fatalf("%v", got)
	}
	r.modem.mu.Lock()
	s := r.modem.sent[0]
	r.modem.mu.Unlock()
	if s.number != "+22890123456" || !strings.HasPrefix(s.text, "ALARME") {
		t.Fatalf("%+v", s)
	}
	// The operator confirms the delivery.
	r.gw.Delivered("line1", "1", true)
	r.waitFor("delivered", func() bool { return len(r.results(ref)) == 3 })
	if got := r.results(ref); got[2] != "delivered/" {
		t.Fatalf("%v", got)
	}
	r.validateAll()
}

func TestACallRequestRingsTheContact(t *testing.T) {
	r := newRig(t, nil)
	ref := r.request(contract.ChannelCall, "u_owner", contract.AlertFire)
	r.waitFor("sent", func() bool { return len(r.results(ref)) == 2 })
	if _, calls, _, _ := r.modem.count(); calls != 1 {
		t.Fatalf("%d calls", calls)
	}
}

func TestRequestsForOtherChannelsAndDuplicatesAreIgnored(t *testing.T) {
	r := newRig(t, nil)
	app := r.request(contract.ChannelApp, "u_owner", contract.AlertIntrusion)
	ref := r.request(contract.ChannelSMS, "u_owner", contract.AlertIntrusion)
	r.waitFor("sent", func() bool { return len(r.results(ref)) == 2 })
	// The same request delivered again by the broker.
	env, _ := contract.NewEnvelope(site, "escalation", 1, ref, t0, contract.TypeAlertRequest, contract.AlertRequestData{
		Incident: "inc1", Kind: contract.AlertIntrusion, Urgency: "high", Contact: "u_owner", Channel: contract.ChannelSMS,
	})
	b, _ := json.Marshal(env)
	r.gw.HandleMessage(Message{Topic: contract.AlertRequestTopic(site), Payload: b})
	// And things that are not alert requests at all.
	r.gw.HandleMessage(Message{Topic: contract.AlertRequestTopic(site), Payload: []byte("{")})
	r.gw.HandleMessage(Message{Topic: contract.AlertAckTopic(site), Payload: b})
	r.gw.HandleMessage(Message{Topic: contract.AlertRequestTopic(site), Payload: bytes.Repeat([]byte("x"), maxPayload+1)})
	other, _ := contract.NewEnvelope("tg-lom-0002", "escalation", 1, r.ids.New(), t0, contract.TypeAlertRequest, contract.AlertRequestData{Contact: "u_owner", Channel: contract.ChannelSMS})
	b, _ = json.Marshal(other)
	r.gw.HandleMessage(Message{Topic: contract.AlertRequestTopic(site), Payload: b})
	r.gw.loop.Do(context.Background(), func(time.Time) []gsm.Action { return nil })
	time.Sleep(20 * time.Millisecond)
	if sent, _, _, _ := r.modem.count(); sent != 1 {
		t.Fatalf("%d SMS", sent)
	}
	if len(r.results(app)) != 0 {
		t.Fatal("an app request is not the gateway's business")
	}
}

func TestAModemFailureIsReportedWithItsReason(t *testing.T) {
	r := newRig(t, nil)
	r.modem.mu.Lock()
	r.modem.sendErr = fmt.Errorf("modem said +CMS ERROR: 21: %w", ErrNoCredit)
	r.modem.mu.Unlock()
	ref := r.request(contract.ChannelSMS, "u_owner", contract.AlertIntrusion)
	r.waitFor("the failure", func() bool { return len(r.results(ref)) == 2 })
	if got := r.results(ref); got[1] != "failed/no_credit" {
		t.Fatalf("%v", got)
	}
	r.waitFor("the status to say so", func() bool { return r.status().Lines[0].Credit == "empty" && r.status().Queued == 1 })
	r.validateAll()
}

func TestReasons(t *testing.T) {
	for err, want := range map[error]gsm.Reason{
		ErrNoNetwork: gsm.ReasonNoNetwork, ErrNoCredit: gsm.ReasonNoCredit, ErrBusy: gsm.ReasonBusy,
		ErrUnreachable: gsm.ReasonUnreachable, ErrTimeout: gsm.ReasonTimeout,
		context.DeadlineExceeded: gsm.ReasonTimeout, errors.New("ERROR"): gsm.ReasonModemError,
	} {
		if got := reason(fmt.Errorf("wrapped: %w", err)); got != want {
			t.Errorf("%v: %s", err, got)
		}
	}
}

func TestAnSMSAnswer1BecomesAnAcknowledgement(t *testing.T) {
	r := newRig(t, nil)
	ref := r.request(contract.ChannelSMS, "u_owner", contract.AlertIntrusion)
	r.waitFor("sent", func() bool { return len(r.results(ref)) == 2 })
	r.gw.Incoming("line1", "+22890123456", "1")
	r.gw.Incoming("line1", "+22899999999", "1") // a stranger
	r.waitFor("the acknowledgement", func() bool { return len(r.broker.ofType(contract.TypeAlertAck)) == 1 })
	d, _ := contract.DecodeData[contract.AlertAckData](r.broker.ofType(contract.TypeAlertAck)[0].env)
	if d.Incident != "inc1" || d.Contact != "u_owner" || d.Via != "sms" {
		t.Fatalf("%+v", d)
	}
	r.waitFor("the stranger to be logged", func() bool { return strings.Contains(r.logs.String(), "unknown number") })
	r.validateAll()
}

func TestASilentModemIsRestarted(t *testing.T) {
	r := newRig(t, nil)
	r.modem.mu.Lock()
	r.modem.checkErr = errors.New("no answer")
	r.modem.mu.Unlock()
	for i := 0; i < 3; i++ {
		_, _, before, _ := r.modem.count()
		r.clock.Advance(30 * time.Second)
		r.waitFor("a check", func() bool { _, _, n, _ := r.modem.count(); return n > before })
	}
	r.waitFor("the restart", func() bool { _, _, _, n := r.modem.count(); return n == 1 })
	r.waitFor("the line back", func() bool { st := r.status(); return st.Lines[0].Responding && st.Lines[0].Restarts == 1 })
	if !strings.Contains(r.logs.String(), "restarting it") {
		t.Fatal("the restart must be logged")
	}
	r.validateAll()
}

func TestTheQueueSurvivesARestartOfTheGateway(t *testing.T) {
	first := newRig(t, nil)
	first.modem.mu.Lock()
	first.modem.sendErr = ErrNoNetwork
	first.modem.mu.Unlock()
	ref := first.request(contract.ChannelSMS, "u_owner", contract.AlertIntrusion)
	first.waitFor("the failure", func() bool { return len(first.results(ref)) == 2 })
	first.gw.loop.Do(context.Background(), func(time.Time) []gsm.Action { return nil })
	first.stop()

	second := newRig(t, first.store)
	second.clock.Advance(10 * time.Second) // past the wait before the next try
	second.waitFor("the SMS after the restart", func() bool { sent, _, _, _ := second.modem.count(); return sent == 1 })
	second.waitFor("sent", func() bool { return len(second.results(ref)) == 1 && second.results(ref)[0] == "sent/" })
}

func TestConfigurationAndDependenciesAreChecked(t *testing.T) {
	good := DefaultConfig(site, gsm.DefaultConfig([]gsm.Line{{ID: "line1"}}, nil))
	deps := Deps{Clock: &fakeClock{now: t0, timers: map[*fakeTimer]bool{}}, Store: &memStore{docs: map[string][]byte{}}, Publisher: &fakeBroker{}, NewID: contract.NewIDGenerator().New,
		Lines: map[string]Modem{"line1": &fakeModem{}}}
	if _, err := New(good, deps); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.Site = "Lome"
	if _, err := New(bad, deps); err == nil {
		t.Error("a bad site")
	}
	bad = good
	bad.StatusEvery = 0
	if _, err := New(bad, deps); err == nil {
		t.Error("no status repetition")
	}
	bad = good
	bad.OutboxSize = 0
	if _, err := New(bad, deps); err == nil {
		t.Error("no outbox")
	}
	bad = good
	bad.GSM.Lines = nil
	if _, err := New(bad, deps); err == nil {
		t.Error("no line")
	}
	noModem := deps
	noModem.Lines = map[string]Modem{}
	if _, err := New(good, noModem); err == nil {
		t.Error("a line without a modem")
	}
	noClock := deps
	noClock.Clock = nil
	if _, err := New(good, noClock); err == nil {
		t.Error("no clock")
	}
	g, _ := New(good, deps)
	if subs := g.Subscriptions(); len(subs) != 1 || subs[0].Filter != contract.AlertRequestTopic(site) || subs[0].QoS != 1 {
		t.Fatalf("%+v", subs)
	}
}
