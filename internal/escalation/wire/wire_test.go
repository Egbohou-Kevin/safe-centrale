package wire

import (
	"encoding/json"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/escalation"
	"safehome/internal/schematest"
)

var now = time.Date(2026, 10, 17, 21, 4, 0, 0, time.UTC)

func TestEveryKindUrgencyAndChannelTranslatesToAValidRequest(t *testing.T) {
	kinds := []escalation.AlertKind{
		escalation.KindIntrusion, escalation.KindFire, escalation.KindPanic, escalation.KindTamper,
		escalation.KindAuthFailed, escalation.KindArmingBypassed, escalation.KindHubBatteryLow,
		escalation.KindMainsRestored, escalation.KindDeviceOffline, escalation.KindBatteryLow, escalation.KindGSMDegraded,
	}
	for _, kind := range kinds {
		for _, ch := range []escalation.Channel{escalation.ChannelApp, escalation.ChannelWhatsApp, escalation.ChannelSMS, escalation.ChannelCall} {
			for _, u := range []escalation.Urgency{escalation.UrgencyCritical, escalation.UrgencyHigh, escalation.UrgencyLow} {
				d, err := Request(escalation.Send{
					Incident: "inc1", Kind: kind, Urgency: u, Wave: 2, Contact: "owner", Channel: ch,
					Devices: []string{"dev_a"}, Percent: 40, Text: "ALARME Boutique",
				})
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
		if _, err := Request(s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestResultAndAck(t *testing.T) {
	for status, want := range map[string]escalation.ResultStatus{
		contract.ResultQueued: escalation.ResultQueued, contract.ResultSent: escalation.ResultSent,
		contract.ResultDelivered: escalation.ResultDelivered, contract.ResultFailed: escalation.ResultFailed,
	} {
		r, ok := Result(contract.AlertResultData{Ref: "x", Status: status}, 7, now)
		if !ok || r.Status != want || r.Key != 7 || !r.Now.Equal(now) {
			t.Errorf("%s: %+v %v", status, r, ok)
		}
	}
	if _, ok := Result(contract.AlertResultData{Ref: "x", Status: "lost"}, 1, now); ok {
		t.Error("an unknown status must be refused")
	}
	if r, ok := ResultByRef(contract.AlertResultData{Ref: "x", Status: contract.ResultSent}, now); !ok ||
		r.Ref != "x" || r.Key != 0 || r.Status != escalation.ResultSent || !r.Now.Equal(now) {
		t.Errorf("%+v %v", r, ok)
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
