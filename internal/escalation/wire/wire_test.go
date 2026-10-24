package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/escalation"
	"safehome/internal/schematest"
)

var now = time.Date(2026, 10, 17, 21, 4, 0, 0, time.UTC)

func TestEveryKindUrgencyAndChannelTranslatesToAValidRequest(t *testing.T) {
	for kind := range escalation.DefaultPlans() { // every kind has a plan
		for _, ch := range []escalation.Channel{escalation.ChannelApp, escalation.ChannelWhatsApp, escalation.ChannelSMS, escalation.ChannelCall} {
			for _, u := range []escalation.Urgency{escalation.UrgencyCritical, escalation.UrgencyHigh, escalation.UrgencyLow} {
				d, err := Request(escalation.Send{
					Incident: "inc1", RaisedAt: now, Kind: kind, Urgency: u, Wave: 2, Contact: "owner", Channel: ch,
					Devices: []string{"dev_a"}, Percent: 40, Text: "ALARME Boutique",
				}, nil)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(d)
				if err := schematest.Check("alert-request.json", raw); err != nil {
					t.Errorf("%s/%s/%s: %v", kind, ch, u, err)
				}
				if d.Contact != "owner" || d.Wave != 2 || d.Text == "" || d.Incident != "inc1" {
					t.Errorf("%+v", d)
				}
			}
		}
	}
}

func TestRequestRefusesWhatTheContractDoesNotKnow(t *testing.T) {
	ok := escalation.Send{Kind: escalation.KindFire, Urgency: escalation.UrgencyHigh, Channel: escalation.ChannelSMS}
	for name, mutate := range map[string]func(*escalation.Send){
		"kind":    func(s *escalation.Send) { s.Kind = "meteor" },
		"urgency": func(s *escalation.Send) { s.Urgency = "whenever" },
		"channel": func(s *escalation.Send) { s.Channel = "pigeon" },
	} {
		s := ok
		mutate(&s)
		if _, err := Request(s, nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// The request says when the incident was raised and in which zone the site lives,
// so that a phone in another zone can still say the site's hour.
func TestARequestCarriesTheIncidentTimeAndTheSiteZone(t *testing.T) {
	lome, err := time.LoadLocation("Africa/Lome")
	if err != nil {
		t.Fatal(err)
	}
	raised := time.Date(2026, 10, 24, 16, 15, 31, 0, time.UTC)
	d, err := Request(escalation.Send{
		Incident: "inc1", RaisedAt: raised, Kind: escalation.KindIntrusion, Urgency: escalation.UrgencyCritical,
		Wave: 2, Contact: "owner", Channel: escalation.ChannelApp,
	}, lome)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d)
	if !strings.Contains(string(raw), `"incident_at":"2026-10-24T16:15:31.000Z","zone":"Africa/Lome"`) {
		t.Fatalf("%s", raw)
	}
	if got := Zone(nil); got != "UTC" {
		t.Fatalf("no zone is UTC, got %q", got)
	}
}

func TestResultAndAck(t *testing.T) {
	for status, want := range map[string]escalation.ResultStatus{
		contract.ResultQueued: escalation.ResultQueued, contract.ResultSent: escalation.ResultSent,
		contract.ResultDelivered: escalation.ResultDelivered, contract.ResultFailed: escalation.ResultFailed,
	} {
		r, ok := ResultByRef(contract.AlertResultData{Ref: "x", Status: status}, now)
		if !ok || r.Ref != "x" || r.Key != 0 || r.Status != want || !r.Now.Equal(now) {
			t.Errorf("%s: %+v %v", status, r, ok)
		}
	}
	for _, d := range []contract.AlertResultData{{Ref: "x", Status: "lost"}, {Status: contract.ResultSent}} {
		if _, ok := ResultByRef(d, now); ok {
			t.Errorf("%+v accepted", d)
		}
	}
	a := Ack(contract.AlertAckData{Incident: "inc1", Contact: "owner", Via: "sms"}, now)
	if a.Incident != "inc1" || a.Contact != "owner" || !a.Now.Equal(now) {
		t.Errorf("%+v", a)
	}
}

func TestEveryPlanHasAContractNameAndBack(t *testing.T) {
	plans := escalation.DefaultPlans()
	if names := KindNames(); len(names) != len(plans) {
		t.Fatalf("%d contract names %v for %d plans", len(names), names, len(plans))
	}
	for k := range plans {
		name, ok := kinds[k]
		if !ok {
			t.Errorf("plan %q has no contract name", k)
			continue
		}
		if back, ok := Kind(name); !ok || back != k {
			t.Errorf("Kind(%q) = %q, %v; want %q", name, back, ok, k)
		}
	}
	if k, ok := Kind(contract.AlertBatteryLowHub); !ok || k != escalation.KindHubBatteryLow {
		t.Errorf("battery_low_hub -> %q", k)
	}
	if _, ok := Kind("flood"); ok {
		t.Error("an unknown kind was found")
	}
}

// An alert that is part of an alarm names it, and says when it is handled; it never
// says handled without naming the alarm, which the schema refuses.
func TestARequestNamesItsAlarmAndSaysWhenItIsHandled(t *testing.T) {
	base := escalation.Send{Incident: "y", RaisedAt: now, Kind: escalation.KindSirenFailed, Urgency: escalation.UrgencyLow,
		Wave: 1, Contact: "owner", Channel: escalation.ChannelSMS, Text: "la sirene"}
	for name, c := range map[string]struct {
		alarm         string
		handled       bool
		incident, ack string // what the request must and must not carry
	}{
		"handled": {"x", true, `"alarm_incident":"x"`, `"alarm_handled":true`},
		"loud":    {"x", false, `"alarm_incident":"x"`, ""},
		"alone":   {"", true, "", ""},
	} {
		s := base
		s.Alarm, s.AlarmHandled = c.alarm, c.handled
		d, err := Request(s, nil)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(d)
		if err := schematest.Check("alert-request.json", raw); err != nil {
			t.Errorf("%s: %v\n%s", name, err, raw)
		}
		for key, want := range map[string]string{"alarm_incident": c.incident, "alarm_handled": c.ack} {
			if got := strings.Contains(string(raw), key); got != (want != "") || !strings.Contains(string(raw), want) {
				t.Errorf("%s: %s, want %s", name, raw, want)
			}
		}
	}
}
