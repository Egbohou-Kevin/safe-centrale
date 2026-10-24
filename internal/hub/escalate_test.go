package hub

import (
	"encoding/json"
	"slices"
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

// The trial on real hardware (24 October 2026): the siren ran out, the system went
// back to armed, then the owner disarmed from the app. The intrusion's next wave must
// not leave after that.
func TestADisarmAfterTheSirenRanOutEndsTheEscalation(t *testing.T) {
	r := newRig(t, nil, func(c *Config) {
		c.Engine.ExitDelay, c.Engine.EntryDelay = 30*time.Second, 15*time.Second
		c.Engine.SirenCycle, c.Engine.SirenPause, c.Engine.SirenCycles = 20*time.Second, 10*time.Second, 2
		c.Escalation = escalation.DefaultConfig("Boutique Essai", []escalation.Contact{
			{ID: "u_kevin", Name: "Kevin", Role: escalation.RoleOwner, Channels: []escalation.Channel{escalation.ChannelApp}},
		})
	})
	r.start()
	r.setCode("u_kevin", "482915")
	r.order(contract.CmdArm, contract.CommandArgs{}, contract.OriginApp, "u_kevin")
	r.mustBe(engine.ModeArming)
	r.clock.Advance(30 * time.Second)
	r.mustBe(engine.ModeArmed)
	r.clock.Advance(28 * time.Second)
	r.device(devDoor, contract.CapContact, "open")
	r.mustBe(engine.ModeEntryDelay)
	r.clock.Advance(15 * time.Second)
	r.mustBe(engine.ModeTriggered)
	r.settle()
	if got := requested(t, r); !slices.Equal(got, []string{"u_kevin/app/1"}) {
		t.Fatalf("first wave: %v", got)
	}

	for range 3 { // 20 s of siren, a 10 s pause, 20 s more: back to armed
		r.clock.Advance(20 * time.Second)
		r.settle()
	}
	r.mustBe(engine.ModeArmed)
	if len(eventsOf(r, contract.TypeAlarmCleared)) != 1 {
		t.Fatal("the siren must have run out")
	}

	r.clock.Advance(20 * time.Second)
	r.order(contract.CmdDisarm, contract.CommandArgs{Code: "482915"}, contract.OriginApp, "u_kevin")
	r.mustBe(engine.ModeDisarmed)
	r.clock.Advance(30 * time.Minute)
	r.settle()
	if got := requested(t, r); len(got) != 1 {
		t.Fatalf("%v: the owner disarmed, nobody must be told any more", got)
	}
	if !strings.Contains(r.logs.String(), "outcome=closed") {
		t.Fatalf("the escalation must end as closed:\n%s", r.logs.String())
	}
}

// The trial on real hardware (24 October 2026) again: the intrusion was raised at
// 16:15:31 in Lome and its second wave left two minutes later. A phone in Paris
// showed the second wave's time, 18:17 there, as the alarm's. Every request of the
// incident now carries when it was raised and the site's zone.
func TestEveryWaveCarriesTheIncidentTimeAndTheSiteZone(t *testing.T) {
	lome, err := time.LoadLocation("Africa/Lome")
	if err != nil {
		t.Fatal(err)
	}
	raised := time.Date(2026, 10, 24, 16, 15, 31, 0, time.UTC)
	start := raised.Add(-(30 + 28 + 15) * time.Second) // the exit delay, the wait, the entry delay
	r := newRigAt(t, start, nil, func(c *Config) {
		c.Engine.ExitDelay, c.Engine.EntryDelay = 30*time.Second, 15*time.Second
		c.Engine.SirenCycle, c.Engine.SirenPause, c.Engine.SirenCycles = 20*time.Second, 10*time.Second, 2
		c.Engine.Location = lome
		c.Escalation = escalation.DefaultConfig("Boutique Essai", []escalation.Contact{
			{ID: "u_kevin", Name: "Kevin", Role: escalation.RoleOwner, Channels: []escalation.Channel{escalation.ChannelApp}},
		})
		c.Escalation.Location = lome
	})
	r.start()
	r.setCode("u_kevin", "482915")
	r.order(contract.CmdArm, contract.CommandArgs{}, contract.OriginApp, "u_kevin")
	r.mustBe(engine.ModeArming)
	r.clock.Advance(30 * time.Second)
	r.mustBe(engine.ModeArmed)
	r.clock.Advance(28 * time.Second)
	r.device(devDoor, contract.CapContact, "open")
	r.mustBe(engine.ModeEntryDelay)
	r.clock.Advance(15 * time.Second)
	r.mustBe(engine.ModeTriggered)
	r.settle()

	// The siren runs out (50 s), then the second wave is due 2 minutes after the alarm.
	for range 6 {
		r.clock.Advance(20 * time.Second)
		r.settle()
	}
	r.clock.Advance(time.Second)
	drivertest.WaitFor(t, "the second wave", func() bool { r.settle(); return len(requested(t, r)) == 2 })

	msgs := eventsOf(r, contract.TypeAlertRequest)
	wantSent := []time.Time{raised, raised.Add(2 * time.Minute)}
	for i, m := range msgs {
		d := dataOf[contract.AlertRequestData](t, m)
		if d.Wave != i+1 || !m.Env.TS.Equal(wantSent[i]) {
			t.Errorf("request %d: wave %d asked at %v, want wave %d at %v", i, d.Wave, m.Env.TS.Time, i+1, wantSent[i])
		}
		if !d.IncidentAt.Equal(raised) || d.Zone != "Africa/Lome" {
			t.Errorf("wave %d: incident_at %v zone %q, want %v Africa/Lome", d.Wave, d.IncidentAt.Time, d.Zone, raised)
		}
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
