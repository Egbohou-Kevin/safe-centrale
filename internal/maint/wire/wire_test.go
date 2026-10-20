package wire

import (
	"encoding/json"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/maint"
	"safehome/internal/schematest"
)

var now = time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)

func TestOrders(t *testing.T) {
	o := Open(contract.MaintOpenData{Session: "m1", User: "kevin", Reason: "modem", DurationS: 1800, Exp: contract.TS(now.Add(time.Minute))}, now)
	if o != (maint.Open{Now: now, Session: "m1", User: "kevin", Length: 30 * time.Minute, Exp: now.Add(time.Minute)}) {
		t.Fatalf("%+v", o)
	}
	if c := Close(contract.MaintCloseData{Session: "m1"}, now); c != (maint.Close{Now: now, Session: "m1"}) {
		t.Fatalf("%+v", c)
	}
}

func TestEveryReportFollowsTheContract(t *testing.T) {
	for _, r := range []maint.Report{
		{Session: "m1", User: "kevin", Status: maint.Opening, Until: now.Add(time.Hour)},
		{Session: "m1", User: "kevin", Status: maint.IsOpen, Until: now.Add(time.Hour)},
		{Session: "m1", User: "kevin", Status: maint.Closed, Reason: maint.Timeout, Duration: time.Hour},
		{Session: "m1", Status: maint.Closed, Reason: maint.Stopped},
		{Session: "m2", Status: maint.Failed, Reason: maint.Busy},
		{Session: "m2", Status: maint.Failed, Reason: maint.Expired},
		{Session: "m2", Status: maint.Failed, Reason: maint.Disabled},
		{Session: "m2", Status: maint.Failed, Reason: maint.Unreachable},
	} {
		d := Session(r)
		raw, _ := json.Marshal(d)
		if err := schematest.Check("maint-session.json", raw); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
		if err := d.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if d := Session(maint.Report{Session: "m", Status: maint.Closed, Duration: 90 * time.Second}); d.DurationS != 90 || d.Until != nil {
		t.Fatalf("%+v", d)
	}
}
