package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/gsm"
	"safehome/internal/inbox"
	"safehome/internal/outbox/outboxtest"
)

func TestTheStatusIsRepeatedEvenWhenNothingChanged(t *testing.T) {
	const every = 47 * time.Second // no other timer of the gateway uses it
	r := newRig(t, nil, func(c *Config) { c.GSM.CheckEvery = time.Hour; c.StatusEvery = every })
	before := len(r.broker.OfType(contract.TypeGSMStatus))
	stamp := func() time.Time {
		all := r.broker.OfType(contract.TypeGSMStatus)
		return all[len(all)-1].Env.TS.Time
	}
	r.clock.PassTimer(t, every)
	drivertest.WaitFor(t, "the repeated status", func() bool { return len(r.broker.OfType(contract.TypeGSMStatus)) == before+1 })
	if !stamp().Equal(t0.Add(every)) {
		t.Fatalf("stamped %v: the hub judges the gateway alive by this time", stamp())
	}
	r.clock.PassTimer(t, every)
	drivertest.WaitFor(t, "once more", func() bool { return len(r.broker.OfType(contract.TypeGSMStatus)) == before+2 })
}

func TestTheLinesStatusIsPublishedRetainedAtStart(t *testing.T) {
	r := newRig(t, nil)
	st := r.status()
	if st.Lines[0].Operator != "TOGOCOM" || !st.Lines[0].Responding || st.Lines[0].Signal != 70 || st.Queued != 0 {
		t.Fatalf("%+v", st)
	}
	all := r.broker.OfType(contract.TypeGSMStatus)
	if !all[len(all)-1].Retain || all[len(all)-1].Topic != contract.GSMStatusTopic(site) {
		t.Fatal("the status is retained, on its topic")
	}
	r.validateAll()
}

func TestAnSMSRequestIsSentAndReported(t *testing.T) {
	r := newRig(t, nil)
	ref := r.request(contract.ChannelSMS, "u_owner", contract.AlertIntrusion)
	drivertest.WaitFor(t, "queued then sent", func() bool { return len(r.results(ref)) == 2 })
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
	drivertest.WaitFor(t, "delivered", func() bool { return len(r.results(ref)) == 3 })
	if got := r.results(ref); got[2] != "delivered/" {
		t.Fatalf("%v", got)
	}
	// Each step leaves a line in the logs, with the request's id.
	for _, want := range []string{"a message is queued", "a line sends a message", "a line sent", "a message was sent", "a message was delivered"} {
		drivertest.WaitFor(t, want, func() bool { return strings.Contains(r.logs.String(), `msg="`+want+`" ref=`+ref) })
	}
	r.validateAll()
}

// The trial on a hub without a modem (25 October 2026): the logs held nothing but the
// line checks, and the end of the alarm said each message "not sent, the alarm was
// over". The logs say the message waits for a line, then why it never left, and so
// does its result.
func TestAMessageThatNoModemSendsIsLoggedAndSaysWhy(t *testing.T) {
	r := newRig(t, nil)
	r.modem.mu.Lock()
	r.modem.checkErr = errors.New("no modem on the port")
	r.modem.mu.Unlock()
	r.clock.Advance(30 * time.Second)
	drivertest.WaitFor(t, "the line down", func() bool { return !r.status().Lines[0].Responding })
	ref := r.request(contract.ChannelSMS, "u_owner", contract.AlertIntrusion)
	drivertest.WaitFor(t, "queued", func() bool { return len(r.results(ref)) == 1 })
	r.publish(contract.AlarmEventTopic(site), r.ids.New(), t0, contract.TypeAlarmDisarmed,
		contract.DisarmedData{Origin: contract.OriginApp, User: "u_owner", Incidents: []string{"inc1"}})
	drivertest.WaitFor(t, "the failure", func() bool { return len(r.results(ref)) == 2 })
	if got := r.results(ref); got[1] != "failed/modem_error" {
		t.Fatalf("%v: the line was down all along, not just the alarm over", got)
	}
	for _, want := range []string{
		`level=WARN msg="a message is queued, no line can send it" ref=` + ref,
		`level=WARN msg="a message was not sent" ref=` + ref + ` reason=modem_error`,
	} {
		if !strings.Contains(r.logs.String(), want) {
			t.Fatalf("the logs miss %q:\n%s", want, r.logs.String())
		}
	}
	r.validateAll()
}

// An SMS is given the domain's whole time to leave, which leaves the modem its own
// 120 s for AT+CMGS and the waits before it: cut at 90 s, an SMS the network took
// longer to accept was reported failed and sent again while it left.
func TestAnSMSIsGivenTheTimeTheModemNeeds(t *testing.T) {
	r := newRig(t, nil)
	ref := r.request(contract.ChannelSMS, "u_owner", contract.AlertIntrusion)
	drivertest.WaitFor(t, "sent", func() bool { return len(r.results(ref)) == 2 })
	r.modem.mu.Lock()
	given := r.modem.given
	r.modem.mu.Unlock()
	if want := r.gw.cfg.GSM.SMSTimeout; len(given) != 1 || given[0] < want-10*time.Second || want < 150*time.Second {
		t.Fatalf("the SMS was given %v of a domain bound %v: the modem needs 120 s and its waits", given, want)
	}
}

func TestACallRequestRingsTheContact(t *testing.T) {
	r := newRig(t, nil)
	ref := r.request(contract.ChannelCall, "u_owner", contract.AlertFire)
	drivertest.WaitFor(t, "sent", func() bool { return len(r.results(ref)) == 2 })
	if _, calls, _, _ := r.modem.count(); calls != 1 {
		t.Fatalf("%d calls", calls)
	}
}

func TestRequestsForOtherChannelsAndDuplicatesAreIgnored(t *testing.T) {
	r := newRig(t, nil)
	app := r.request(contract.ChannelApp, "u_owner", contract.AlertIntrusion)
	ref := r.request(contract.ChannelSMS, "u_owner", contract.AlertIntrusion)
	drivertest.WaitFor(t, "sent", func() bool { return len(r.results(ref)) == 2 })
	// The same request delivered again by the broker.
	r.publish(contract.AlertRequestTopic(site), ref, t0, contract.TypeAlertRequest, contract.AlertRequestData{
		Incident: "inc1", Kind: contract.AlertIntrusion, Urgency: contract.UrgencyCritical, Contact: "u_owner", Channel: contract.ChannelSMS,
		Text: "ALARME Boutique: intrusion a 21:04. Repondre 1 si vous gerez.",
	})
	env, _ := contract.NewEnvelope(site, "escalation", 1, ref, t0, contract.TypeAlertRequest, contract.AlertRequestData{})
	b, _ := json.Marshal(env)
	// And things that are not alert requests at all.
	r.gw.HandleMessage(inbox.Message{Topic: contract.AlertRequestTopic(site), Payload: []byte("{")})
	r.gw.HandleMessage(inbox.Message{Topic: contract.AlertAckTopic(site), Payload: b})
	r.gw.HandleMessage(inbox.Message{Topic: contract.AlertRequestTopic(site), Payload: bytes.Repeat([]byte("x"), maxPayload+1)})
	other, _ := contract.NewEnvelope("tg-lom-0002", "escalation", 1, r.ids.New(), t0, contract.TypeAlertRequest, contract.AlertRequestData{Contact: "u_owner", Channel: contract.ChannelSMS})
	b, _ = json.Marshal(other)
	r.gw.HandleMessage(inbox.Message{Topic: contract.AlertRequestTopic(site), Payload: b})
	r.settle()
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
	drivertest.WaitFor(t, "the failure", func() bool { return len(r.results(ref)) == 2 })
	if got := r.results(ref); got[1] != "failed/no_credit" {
		t.Fatalf("%v", got)
	}
	drivertest.WaitFor(t, "the status to say so", func() bool { return r.status().Lines[0].Credit == "empty" && r.status().Queued == 1 })
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
	drivertest.WaitFor(t, "sent", func() bool { return len(r.results(ref)) == 2 })
	r.gw.Incoming("line1", "+22890123456", "1", time.Time{})
	r.gw.Incoming("line1", "+22899999999", "1", time.Time{}) // a stranger
	drivertest.WaitFor(t, "the acknowledgement", func() bool { return len(r.broker.OfType(contract.TypeAlertAck)) == 1 })
	d, _ := contract.DecodeData[contract.AlertAckData](r.broker.OfType(contract.TypeAlertAck)[0].Env)
	if d.Incident != "inc1" || d.Contact != "u_owner" || d.Via != "sms" {
		t.Fatalf("%+v", d)
	}
	drivertest.WaitFor(t, "the stranger to be logged", func() bool { return strings.Contains(r.logs.String(), "unknown number") })
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
		drivertest.WaitFor(r.t, "a check", func() bool { _, _, n, _ := r.modem.count(); return n > before })
	}
	drivertest.WaitFor(t, "the restart", func() bool { _, _, _, n := r.modem.count(); return n == 1 })
	drivertest.WaitFor(t, "the line back", func() bool { st := r.status(); return st.Lines[0].Responding && st.Lines[0].Restarts == 1 })
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
	drivertest.WaitFor(t, "the failure", func() bool { return len(first.results(ref)) == 2 })
	first.gw.loop.Do(context.Background(), func(time.Time) []gsm.Action { return nil })
	first.stop()

	second := newRig(t, first.store)
	second.clock.Advance(10 * time.Second) // past the wait before the next try
	drivertest.WaitFor(t, "the SMS after the restart", func() bool { sent, _, _, _ := second.modem.count(); return sent == 1 })
	drivertest.WaitFor(t, "sent", func() bool { return len(second.results(ref)) == 1 && second.results(ref)[0] == "sent/" })
}

func TestConfigurationAndDependenciesAreChecked(t *testing.T) {
	good := DefaultConfig(site, gsm.DefaultConfig([]gsm.Line{{ID: "line1"}}, nil))
	deps := Deps{Clock: drivertest.NewFakeClock(t0), Store: drivertest.NewMemStore(), Publisher: &outboxtest.Broker{}, NewID: contract.NewIDGenerator().New,
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
	bad.Outbox.Size = 0
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
	want := []inbox.Subscription{
		{Filter: contract.AlertRequestTopic(site), QoS: 1},
		{Filter: contract.AlarmEventTopic(site), QoS: 1},
		{Filter: contract.AlertAckTopic(site), QoS: 1},
	}
	if subs := g.Subscriptions(); !slices.Equal(subs, want) {
		t.Fatalf("%+v", subs)
	}
}

func TestARequestHeldByTheBrokerTooLongIsGivenUp(t *testing.T) {
	r := newRig(t, nil)
	// Written eleven minutes ago, while the gateway was down.
	ref := r.requestAt(t0.Add(-11*time.Minute), contract.ChannelSMS, "u_owner", contract.AlertIntrusion)
	drivertest.WaitFor(t, "the failure", func() bool { return len(r.results(ref)) == 1 })
	if got := r.results(ref); got[0] != "failed/timeout" {
		t.Fatalf("%v", got)
	}
	if sent, _, _, _ := r.modem.count(); sent != 0 {
		t.Fatal("an alert too old must not go out")
	}
}

// failingRequest queues an SMS of incident inc1 that waits to be retried.
func failingRequest(r *rig) {
	r.t.Helper()
	r.modem.mu.Lock()
	r.modem.sendErr = ErrNoNetwork
	r.modem.mu.Unlock()
	ref := r.request(contract.ChannelSMS, "u_owner", contract.AlertIntrusion)
	drivertest.WaitFor(r.t, "the failure", func() bool { return len(r.results(ref)) == 2 })
	r.modem.mu.Lock()
	r.modem.sendErr = nil // the network is back
	r.modem.mu.Unlock()
}

// noSMSAfterRetry lets the retry time pass and checks that nothing left.
func noSMSAfterRetry(r *rig) {
	r.t.Helper()
	drivertest.WaitFor(r.t, "the queue to be empty", func() bool { return r.status().Queued == 0 })
	r.clock.Advance(time.Minute)
	// A tick on the loop, whether its timer has fired yet or not: what is due goes now.
	r.gw.loop.Do(context.Background(), func(now time.Time) []gsm.Action { return r.gw.core.Handle(gsm.Tick{Now: now}) })
	r.settle()
	if sent, _, _, _ := r.modem.count(); sent != 0 {
		r.t.Fatal("an SMS went out after the incident ended")
	}
	if !strings.Contains(r.logs.String(), "incident is over") {
		r.t.Fatal("the drop must be logged")
	}
}

func TestADisarmDropsTheWaitingMessages(t *testing.T) {
	r := newRig(t, nil)
	failingRequest(r)
	// The siren cycles running out is not the end of the incident.
	r.publish(contract.AlarmEventTopic(site), r.ids.New(), t0, contract.TypeAlarmCleared,
		contract.ClearedData{Incident: "inc1", Origin: contract.OriginTimeout, DurationS: 300})
	stillWaits(r)
	r.publish(contract.AlarmEventTopic(site), r.ids.New(), t0, contract.TypeAlarmCleared,
		contract.ClearedData{Incident: "inc1", Origin: contract.OriginUser, User: "u_owner", DurationS: 60})
	noSMSAfterRetry(r)
}

// The trial on real hardware (24 October 2026): the alarm went off, its siren ran
// out (alarm.cleared, origin timeout), then the owner disarmed from the app. The
// disarm ended the intrusion: its messages still waiting must not go.
func TestADisarmAfterTheSirenRanOutDropsTheWaitingMessages(t *testing.T) {
	r := newRig(t, nil)
	failingRequest(r)
	r.publish(contract.AlarmEventTopic(site), r.ids.New(), t0, contract.TypeAlarmCleared,
		contract.ClearedData{Incident: "inc1", Origin: contract.OriginTimeout, DurationS: 50})
	stillWaits(r)
	// A disarm that ended other incidents leaves this one alone.
	r.publish(contract.AlarmEventTopic(site), r.ids.New(), t0, contract.TypeAlarmDisarmed,
		contract.DisarmedData{Origin: contract.OriginApp, User: "u_owner", Incidents: []string{"inc0"}})
	stillWaits(r)
	r.publish(contract.AlarmEventTopic(site), r.ids.New(), t0, contract.TypeAlarmDisarmed,
		contract.DisarmedData{Origin: contract.OriginApp, User: "u_owner", Incidents: []string{"inc1"}})
	noSMSAfterRetry(r)
}

func TestAnAcknowledgementDropsTheWaitingMessages(t *testing.T) {
	r := newRig(t, nil)
	failingRequest(r)
	r.publish(contract.AlertAckTopic(site), r.ids.New(), t0, contract.TypeAlertAck,
		contract.AlertAckData{Incident: "inc1", Contact: "u_owner", Via: contract.ChannelApp})
	noSMSAfterRetry(r)
}

// The escalation ignores an acknowledgement from someone who is not a contact, and
// goes on: the gateway must not mute the SMS backup for it. The contacts are those
// with a number, or the list given (some are reached by the app only).
func TestAnAcknowledgementFromAStrangerDropsNothing(t *testing.T) {
	for name, contacts := range map[string][]string{"contacts with a number": nil, "contacts given": {"u_owner", "u_deputy"}} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil, func(c *Config) { c.Contacts = contacts })
			failingRequest(r)
			r.publish(contract.AlertAckTopic(site), r.ids.New(), t0, contract.TypeAlertAck,
				contract.AlertAckData{Incident: "inc1", Contact: "u_stranger", Via: contract.ChannelApp})
			stillWaits(r)
		})
	}
}

// stillWaits checks that the message of the failing request still waits: nothing
// dropped it, and the status published says so.
func stillWaits(r *rig) {
	r.t.Helper()
	r.settle()
	drivertest.WaitFor(r.t, "the outbox to empty", func() bool { return r.gw.out.Depth() == 0 })
	if r.status().Queued != 1 || strings.Contains(r.logs.String(), "incident is over") {
		r.t.Fatal("the message must still wait")
	}
}

// A contact reached by the app only has no number, but is a contact all the same.
func TestAnAcknowledgementFromAContactWithoutANumberDropsTheMessages(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.Contacts = []string{"u_owner", "u_deputy"} })
	failingRequest(r)
	r.publish(contract.AlertAckTopic(site), r.ids.New(), t0, contract.TypeAlertAck,
		contract.AlertAckData{Incident: "inc1", Contact: "u_deputy", Via: contract.ChannelApp})
	noSMSAfterRetry(r)
}

// A message that arrives while the gateway stops (here during its last flush, which
// the broker holds) is given up without an error.
func TestMessagesDuringShutdownAreGivenUpQuietly(t *testing.T) {
	r := newRig(t, nil)
	r.settle()
	release := r.broker.Hold()
	r.gw.repeatStatus(context.Background()) // something left for the last flush
	r.cancel()
	drivertest.WaitFor(t, "the loop to stop", func() bool { return r.gw.Alive(context.Background()) != nil })
	r.request(contract.ChannelSMS, "u_owner", contract.AlertIntrusion)
	r.gw.Incoming("line1", "+22890123456", "1", time.Time{})
	release()
	<-r.done
	if strings.Contains(r.logs.String(), "level=ERROR") {
		t.Fatal("an error was logged for a message given up at shutdown")
	}
}
