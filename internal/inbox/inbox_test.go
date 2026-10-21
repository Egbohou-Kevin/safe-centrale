package inbox

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"safehome/internal/contract"
)

const site = "tg-lom-0001"

func message(t *testing.T, site, typ string, data any) Message {
	t.Helper()
	env, err := contract.NewEnvelope(site, "test", 1, contract.NewIDGenerator().New(), time.Now(), typ, data)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	return Message{Topic: contract.AlarmCmdTopic(site), Payload: b}
}

func TestOpenGivesTheCheckedData(t *testing.T) {
	m := message(t, site, contract.TypeAlarmCommand, contract.AlarmCommandData{Command: contract.CmdDisarm, Origin: contract.OriginApp, User: "u_owner"})
	env, d, err := Open[contract.AlarmCommandData](m, site, contract.TypeAlarmCommand)
	if err != nil || env.Type != contract.TypeAlarmCommand || d.Command != contract.CmdDisarm {
		t.Fatalf("%+v %+v %v", env, d, err)
	}
}

func TestOpenRefuses(t *testing.T) {
	good := contract.AlarmCommandData{Command: contract.CmdDisarm, Origin: contract.OriginApp, User: "u_owner"}
	cases := map[string]struct {
		m    Message
		want error
	}{
		"not an envelope": {Message{Payload: []byte(`{"v":1`)}, contract.ErrInvalid},
		"another site":    {message(t, "tg-lom-0002", contract.TypeAlarmCommand, good), ErrOtherSite},
		"another type":    {message(t, site, contract.TypeAlarmAck, good), ErrWrongType},
		"invalid data":    {message(t, site, contract.TypeAlarmCommand, contract.AlarmCommandData{Command: "dance"}), contract.ErrInvalid},
		"damaged data":    {message(t, site, contract.TypeAlarmCommand, map[string]int{"command": 1}), contract.ErrInvalid},
	}
	for name, c := range cases {
		if _, _, err := Open[contract.AlarmCommandData](c.m, site, contract.TypeAlarmCommand); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
}

func TestEnvelopeTakesAnyOfItsTypes(t *testing.T) {
	m := message(t, site, contract.TypeVideoStop, map[string]string{})
	if _, err := Envelope(m, site, contract.TypeVideoRequest, contract.TypeVideoStop); err != nil {
		t.Fatal(err)
	}
	if _, err := Envelope(m, site, contract.TypeVideoRequest); !errors.Is(err, ErrWrongType) {
		t.Fatalf("got %v", err)
	}
}

func TestRecentIDsForgetsTheOldest(t *testing.T) {
	r := NewRecentIDs(2)
	if !r.FirstTime("a") || r.FirstTime("a") || !r.FirstTime("b") || !r.FirstTime("c") {
		t.Fatal("first sightings")
	}
	if !r.FirstTime("a") {
		t.Fatal("a was forgotten when c arrived")
	}
}

func TestRecentIDsOfSizeZeroDoesNotPanic(t *testing.T) {
	r := NewRecentIDs(0)
	if !r.FirstTime("a") || r.FirstTime("a") {
		t.Fatal("it still remembers the last id")
	}
}
