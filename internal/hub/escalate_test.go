package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/engine"
	"safehome/internal/escalation"
)

// withContacts gives the site an owner and a guard who has no internet.
func withContacts(c *Config) {
	c.Escalation = escalation.DefaultConfig("Test Shop", []escalation.Contact{
		{ID: "u_owner", Name: "Owner", Role: escalation.RoleOwner, Channels: []escalation.Channel{escalation.ChannelApp, escalation.ChannelSMS}},
		{ID: "u_guard", Name: "Guard", Role: escalation.RoleGuard, Channels: []escalation.Channel{escalation.ChannelSMS, escalation.ChannelCall}},
	})
}

// requested lists the alert requests published so far as "contact/channel/wave".
func requested(t *testing.T, r *rig) []string {
	t.Helper()
	var out []string
	for _, m := range eventsOf(r, contract.TypeAlertRequest) {
		d := dataOf[contract.AlertRequestData](t, m)
		out = append(out, d.Contact+"/"+d.Channel+"/"+itoa(d.Wave))
	}
	return out
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// trigger arms the alarm and forces the back door.
func trigger(r *rig) {
	r.order(contract.CmdArm, contract.CommandArgs{}, contract.OriginApp, "u_owner")
	r.mustBe(engine.ModeArming)
	r.clock.Advance(2 * time.Minute)
	r.mustBe(engine.ModeArmed)
	r.device(devBack, contract.CapContact, "open")
	r.mustBe(engine.ModeTriggered)
	r.settle()
}

func requestIDs(t *testing.T, r *rig) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, m := range eventsOf(r, contract.TypeAlertRequest) {
		d := dataOf[contract.AlertRequestData](t, m)
		ids[d.Contact+"/"+d.Channel] = m.Env.ID
	}
	return ids
}

func TestAnIntrusionTellsEachContactByTheRightChannel(t *testing.T) {
	r := newRig(t, nil, withContacts)
	r.start()
	trigger(r)
	// The owner by internet, the guard (no internet) by SMS and call at once.
	if got := requested(t, r); !sameSet(got, []string{"u_owner/app/1", "u_guard/sms/1", "u_guard/call/1"}) {
		t.Fatalf("%v", got)
	}
	// The cloud did not confirm: after 10 s the owner gets the SMS too.
	r.clock.Advance(10 * time.Second)
	drivertest.WaitFor(t, "the SMS to the owner", func() bool { r.settle(); return len(requested(t, r)) == 4 })
	r.validateAll()
}

func TestAnInternetConfirmationHoldsTheSMSBack(t *testing.T) {
	r := newRig(t, nil, withContacts)
	r.start()
	trigger(r)
	id := requestIDs(t, r)["u_owner/app"]
	r.send(contract.AlertResultTopic(site), contract.TypeAlertResult, contract.AlertResultData{Ref: id, Status: contract.ResultSent})
	r.settle()
	r.clock.Advance(10 * time.Second)
	r.settle()
	if got := requested(t, r); len(got) != 3 {
		t.Fatalf("%v: the cloud took the message, no SMS yet", got)
	}
	// Nobody answered after a minute: the SMS doubles the notification.
	r.clock.Advance(time.Minute)
	drivertest.WaitFor(t, "the doubling SMS", func() bool { r.settle(); return len(requested(t, r)) == 4 })
}

func TestAFailedSMSBringsTheNextWaveForward(t *testing.T) {
	r := newRig(t, nil, func(c *Config) {
		c.Escalation = escalation.DefaultConfig("Test Shop", []escalation.Contact{
			{ID: "u_owner", Role: escalation.RoleOwner, Channels: []escalation.Channel{escalation.ChannelSMS}},
			{ID: "u_boss", Role: escalation.RoleManager, Channels: []escalation.Channel{escalation.ChannelSMS}},
		})
	})
	r.start()
	trigger(r)
	id := requestIDs(t, r)["u_owner/sms"]
	r.send(contract.AlertResultTopic(site), contract.TypeAlertResult, contract.AlertResultData{Ref: id, Status: contract.ResultFailed, Reason: "no_credit"})
	// The owner cannot be reached: the next wave starts without waiting its two minutes.
	drivertest.WaitFor(t, "the next wave", func() bool { r.settle(); return len(requested(t, r)) == 2 })
	if got := requested(t, r); got[1] != "u_owner/sms/2" {
		t.Fatalf("%v", got)
	}
}

func TestAnAcknowledgementStopsTheEscalation(t *testing.T) {
	r := newRig(t, nil, withContacts)
	r.start()
	trigger(r)
	incident := dataOf[contract.AlertRequestData](t, eventsOf(r, contract.TypeAlertRequest)[0]).Incident
	r.send(contract.AlertAckTopic(site), contract.TypeAlertAck, contract.AlertAckData{Incident: incident, Contact: "u_guard", Via: "sms"})
	r.settle()
	// The siren does not stop for an acknowledgement.
	r.mustBe(engine.ModeTriggered)
	n := len(requested(t, r))
	r.clock.Advance(30 * time.Minute)
	r.settle()
	if got := requested(t, r); len(got) != n {
		t.Fatalf("%v: nothing must leave after an acknowledgement", got)
	}
	if !strings.Contains(r.logs.String(), "outcome=acknowledged") {
		t.Fatalf("the outcome must be logged:\n%s", r.logs.String())
	}
}

func TestDisarmingEndsTheEscalation(t *testing.T) {
	r := newRig(t, nil, withContacts)
	r.start()
	r.setCode("u_owner", "482915")
	trigger(r)
	n := len(requested(t, r))
	r.order(contract.CmdDisarm, contract.CommandArgs{Code: "482915"}, contract.OriginApp, "u_owner")
	r.mustBe(engine.ModeDisarmed)
	r.clock.Advance(30 * time.Minute)
	r.settle()
	if got := requested(t, r); len(got) != n {
		t.Fatalf("%v: the incident is over, nobody must be told any more", got)
	}
}

func agentStatus(r *rig, uplink string) {
	r.send(contract.AgentStatusTopic(site), contract.TypeAgentStatus, contract.AgentStatusData{Uplink: uplink})
}

func TestWithoutInternetTheGSMChannelsTakeOverAtOnce(t *testing.T) {
	r := newRig(t, nil, withContacts)
	r.start()
	agentStatus(r, contract.UplinkDown)
	r.settle()
	trigger(r)
	if got := requested(t, r); !sameSet(got, []string{"u_owner/sms/1", "u_guard/sms/1", "u_guard/call/1"}) {
		t.Fatalf("%v", got)
	}
	// The link returns: what the internet could not carry goes out.
	agentStatus(r, contract.UplinkUp)
	drivertest.WaitFor(t, "the app message", func() bool { r.settle(); return contains(requested(t, r), "u_owner/app/1") })
}

func TestStrangeResultsAndAcknowledgementsAreIgnored(t *testing.T) {
	r := newRig(t, nil, withContacts)
	r.start()
	trigger(r)
	n := len(requested(t, r))
	id := requestIDs(t, r)["u_owner/app"]

	r.send(contract.AlertResultTopic(site), contract.TypeAlertResult, contract.AlertResultData{Ref: "01NOSUCHREQUEST", Status: contract.ResultFailed})
	r.send(contract.AlertResultTopic(site), contract.TypeAlertResult, contract.AlertResultData{Ref: id, Status: "exploded"})
	r.send(contract.AlertResultTopic(site), contract.TypeAlertAck, contract.AlertAckData{}) // wrong type for the topic
	r.send(contract.AlertResultTopic(site), contract.TypeAlertResult, map[string]any{"ref": 12})
	r.send(contract.AlertAckTopic(site), contract.TypeAlertAck, contract.AlertAckData{Incident: "nope", Contact: "u_owner", Via: "app"})
	r.send(contract.AlertAckTopic(site), contract.TypeAlertAck, contract.AlertAckData{Incident: "x", Contact: "stranger", Via: "sms"})
	r.send(contract.AlertAckTopic(site), contract.TypeAlertAck, map[string]any{"incident": 3})
	r.settle()
	if got := requested(t, r); len(got) != n {
		t.Fatalf("%v", got)
	}
	r.mustBe(engine.ModeTriggered)
	if len(r.hub.escal.esc.Snapshot().Incidents) == 0 {
		t.Fatal("the escalation must go on")
	}
}

func TestTheEscalationGoesOnAfterARestart(t *testing.T) {
	first := newRig(t, nil, withContacts)
	first.start()
	trigger(first)
	first.settle()
	first.stop()

	// The power cut and came back later: wave 2 was due two minutes after the alert,
	// which was raised two minutes after the clock started.
	second := newRig(t, first.store, withContacts)
	second.clock.Advance(5 * time.Minute)
	second.start()
	drivertest.WaitFor(t, "wave 2 after the restart", func() bool {
		second.settle()
		for _, m := range eventsOf(second, contract.TypeAlertRequest) {
			if dataOf[contract.AlertRequestData](t, m).Wave == 2 {
				return true
			}
		}
		return false
	})
}

func TestAResultOfARequestSentBeforeARestartStillCounts(t *testing.T) {
	first := newRig(t, nil, withContacts)
	first.start()
	trigger(first)
	id := requestIDs(t, first)["u_owner/app"]
	first.stop()

	// The hub restarts at once; the cloud's answer to the app message comes after.
	second := newRig(t, first.store, withContacts)
	second.clock.Advance(2 * time.Minute) // the time of the alert
	second.start()
	second.send(contract.AlertResultTopic(site), contract.TypeAlertResult, contract.AlertResultData{Ref: id, Status: contract.ResultSent})
	second.settle()
	second.clock.Advance(10 * time.Second)
	second.settle()
	if got := requested(t, second); len(got) != 0 {
		t.Fatalf("%v: the cloud took the message, no SMS yet", got)
	}
}

func TestEscalationConfigurationIsChecked(t *testing.T) {
	cfg := testConfig()
	cfg.Escalation.Contacts = []escalation.Contact{{ID: "x", Role: "king", Channels: []escalation.Channel{escalation.ChannelSMS}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a bad contact must be refused at configuration time")
	}
}
