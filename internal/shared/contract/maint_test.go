package contract

import (
	"strings"
	"testing"
	"time"
)

func TestMaintenanceMessagesAreChecked(t *testing.T) {
	exp := TS(time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC))
	good := MaintOpenData{Session: "m1", User: "kevin", Reason: "modem", DurationS: 600, Exp: exp}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*MaintOpenData){
		"no session":  func(d *MaintOpenData) { d.Session = "" },
		"no user":     func(d *MaintOpenData) { d.User = "" },
		"no reason":   func(d *MaintOpenData) { d.Reason = "" },
		"long reason": func(d *MaintOpenData) { d.Reason = strings.Repeat("x", 201) },
		"too short":   func(d *MaintOpenData) { d.DurationS = 59 },
		"two hours":   func(d *MaintOpenData) { d.DurationS = 7200 },
		"no exp":      func(d *MaintOpenData) { d.Exp = Timestamp{} },
	} {
		d := good
		change(&d)
		if d.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if (MaintCloseData{}).Validate() == nil || (MaintCloseData{Session: "m"}).Validate() != nil {
		t.Fatal("close")
	}
	for _, bad := range []MaintSessionData{{Status: MaintOpen}, {Session: "m", Status: "ajar"}, {Session: "m", Status: MaintClosed, Reason: "bored"}} {
		if bad.Validate() == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if err := (MaintSessionData{Session: "m", Status: MaintClosed, Reason: MaintTimeout, DurationS: 3600}).Validate(); err != nil {
		t.Fatal(err)
	}
}
