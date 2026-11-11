package contract

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func goodMove() MoveData {
	return MoveData{URLs: []string{"mqtts://b.safehome.example:8883", "wss://b.safehome.example:443/mqtt"},
		Secret: "s3cret-for-cell-b-0001", Until: TS(time.Date(2026, 10, 30, 21, 5, 0, 0, time.UTC))}
}

func TestAMoveOrderIsChecked(t *testing.T) {
	if err := goodMove().Validate(); err != nil {
		t.Fatal(err)
	}
	if goodMove().GiveUp() != 10*time.Minute {
		t.Fatal("the default way back is not after 600 s")
	}
	for name, change := range map[string]func(*MoveData){
		"no url": func(d *MoveData) { d.URLs = nil },
		"five urls": func(d *MoveData) {
			d.URLs = []string{"mqtts://a:1", "mqtts://b:1", "mqtts://c:1", "mqtts://d:1", "mqtts://e:1"}
		},
		"plain mqtt":        func(d *MoveData) { d.URLs = []string{"mqtt://b.example:1883"} },
		"credentials":       func(d *MoveData) { d.URLs = []string{"mqtts://u:pw@b.example:8883"} },
		"no host":           func(d *MoveData) { d.URLs = []string{"mqtts:///x"} },
		"no secret":         func(d *MoveData) { d.Secret = "" },
		"short secret":      func(d *MoveData) { d.Secret = "0123456789abcde" },
		"secret with space": func(d *MoveData) { d.Secret = "0123456789 abcdef" },
		"long secret":       func(d *MoveData) { d.Secret = strings.Repeat("x", 257) },
		"no until":          func(d *MoveData) { d.Until = Timestamp{} },
		"too short a try":   func(d *MoveData) { d.GiveUpAfter = 59 },
		"too long a try":    func(d *MoveData) { d.GiveUpAfter = 3601 },
	} {
		d := goodMove()
		change(&d)
		err := d.Validate()
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: accepted (%v)", name, err)
			continue
		}
		if strings.Contains(err.Error(), "pw") || (d.Secret != "" && strings.Contains(err.Error(), d.Secret)) {
			t.Errorf("%s: the error quotes a secret: %v", name, err)
		}
	}
}

func TestAMoveOrderNeverPrintsItsSecret(t *testing.T) {
	d := goodMove()
	for _, s := range []string{fmt.Sprint(d), fmt.Sprintf("%v %+v %#v %s", d, d, d, d)} {
		if strings.Contains(s, d.Secret) {
			t.Fatalf("printed: %s", s)
		}
	}
}

func TestAMoveReportSaysWhyItWentBack(t *testing.T) {
	for _, ok := range []MovedData{{Ref: "m", To: MovedToB}, {Ref: "m", To: MovedBack, Reason: MoveUnreachable}, {Ref: "m", To: MovedBack, Reason: MoveNoSave}} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []MovedData{{To: MovedToB}, {Ref: "m", To: MovedBack}, {Ref: "m", To: MovedToB, Reason: MoveUnreachable}, {Ref: "m", To: "c"}} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v accepted", bad)
		}
	}
}
