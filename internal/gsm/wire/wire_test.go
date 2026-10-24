package wire

import (
	"encoding/json"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/gsm"
	"safehome/internal/schematest"
)

var now = time.Date(2026, 10, 17, 21, 4, 0, 0, time.UTC)

func TestRequest(t *testing.T) {
	for kind, want := range map[string]gsm.Priority{
		contract.AlertFire: gsm.PriorityLifeSafety, contract.AlertPanic: gsm.PriorityLifeSafety,
		contract.AlertIntrusion: gsm.PriorityIntrusion, contract.AlertSirenFailed: gsm.PriorityIntrusion, contract.AlertTamper: gsm.PriorityOther,
		contract.AlertMainsRestored: gsm.PriorityOther,
	} {
		r, ok := Request(contract.AlertRequestData{Incident: "i", Kind: kind, Urgency: "high", Contact: "owner", Channel: contract.ChannelSMS, Text: "x"}, "id1", now)
		if !ok || r.Priority != want || r.Channel != gsm.ChannelSMS || r.ID != "id1" || r.Contact != "owner" || r.Text != "x" || r.Incident != "i" || !r.Asked.Equal(now) || !r.Now.IsZero() {
			t.Errorf("%s: %+v %v", kind, r, ok)
		}
	}
	if r, ok := Request(contract.AlertRequestData{Kind: contract.AlertFire, Contact: "c", Channel: contract.ChannelCall}, "id", now); !ok || r.Channel != gsm.ChannelCall {
		t.Fatalf("%+v", r)
	}
	for name, d := range map[string]contract.AlertRequestData{
		"app":        {Contact: "c", Channel: contract.ChannelApp},
		"whatsapp":   {Contact: "c", Channel: contract.ChannelWhatsApp},
		"no channel": {Contact: "c"},
		"no contact": {Channel: contract.ChannelSMS},
	} {
		if _, ok := Request(d, "id", now); ok {
			t.Errorf("%s accepted", name)
		}
	}
	if _, ok := Request(contract.AlertRequestData{Contact: "c", Channel: contract.ChannelSMS}, "", now); ok {
		t.Error("a request without id cannot be reported on")
	}
}

func TestResultsAcksAndStatusAreValid(t *testing.T) {
	for _, r := range []gsm.Report{
		{Ref: "r", Status: gsm.StatusQueued}, {Ref: "r", Status: gsm.StatusSent},
		{Ref: "r", Status: gsm.StatusDelivered}, {Ref: "r", Status: gsm.StatusFailed, Reason: gsm.ReasonNoCredit},
	} {
		raw, _ := json.Marshal(Result(r))
		if err := schematest.Check("alert-result.json", raw); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if got := Result(gsm.Report{Ref: "r", Status: gsm.StatusFailed, Reason: gsm.ReasonBusy}); got.Reason != "busy" || got.Status != "failed" {
		t.Fatalf("%+v", got)
	}
	raw, _ := json.Marshal(Ack(gsm.Acknowledge{Incident: "i", Contact: "owner"}))
	if err := schematest.Check("alert-ack.json", raw); err != nil {
		t.Fatal(err)
	}
	ok := true
	st := Status([]gsm.LineStatus{
		{ID: "line1", Operator: "togocom", Responding: true, Registered: true, Signal: 70, Credit: gsm.CreditOK, LastTest: now, LastTestOK: &ok, Restarts: 2},
		{ID: "line2"},
	}, 3)
	raw, _ = json.Marshal(st)
	if err := schematest.Check("gsm-status.json", raw); err != nil {
		t.Fatal(err)
	}
	if st.Queued != 3 || st.Lines[0].LastTest == nil || st.Lines[1].LastTest != nil || st.Lines[0].Restarts != 2 ||
		st.Lines[0].Credit != contract.CreditOK || st.Lines[1].Credit != "" {
		t.Fatalf("%+v", st)
	}
}

func TestCancel(t *testing.T) {
	if c, ok := Cleared(contract.ClearedData{Incident: "i", Origin: contract.OriginUser}); !ok || c.Incident != "i" {
		t.Fatalf("disarmed: %+v %v", c, ok)
	}
	if _, ok := Cleared(contract.ClearedData{Incident: "i", Origin: contract.OriginTimeout}); ok {
		t.Fatal("the siren cycles ran out: the escalation goes on")
	}
	if _, ok := Cleared(contract.ClearedData{Origin: contract.OriginUser}); ok {
		t.Fatal("no incident")
	}
	// A disarm ends what it names, the intrusion whose siren already ran out included.
	if got := Disarmed(contract.DisarmedData{Origin: contract.OriginApp, Incidents: []string{"i", "", "j"}}); len(got) != 2 ||
		got[0].Incident != "i" || got[1].Incident != "j" {
		t.Fatalf("disarmed: %+v", got)
	}
	if got := Disarmed(contract.DisarmedData{Origin: contract.OriginKeypad, Incidents: []string{}}); len(got) != 0 {
		t.Fatalf("a disarm that ended nothing: %+v", got)
	}
	if c, ok := Acked(contract.AlertAckData{Incident: "i", Contact: "owner", Via: contract.ChannelApp}); !ok || c.Incident != "i" {
		t.Fatalf("acknowledged: %+v %v", c, ok)
	}
	if _, ok := Acked(contract.AlertAckData{Contact: "owner"}); ok {
		t.Fatal("no incident")
	}
}
