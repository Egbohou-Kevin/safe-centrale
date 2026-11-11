package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestAlarmCommandValidate(t *testing.T) {
	if err := (AlarmCommandData{Command: CmdArm, Origin: OriginApp}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []AlarmCommandData{
		{Command: "", Origin: OriginApp},
		{Command: CmdArm, Origin: ""},
		{Command: CmdArm, Origin: "sms"}, // no SMS orders in v1
	} {
		if err := d.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v should be refused, got %v", d, err)
		}
	}
	if !NeedsCode(CmdDisarm) || !NeedsCode(CmdSilence) || NeedsCode(CmdArm) {
		t.Error("only disarm and silence need a code")
	}
}

// The access code must appear neither in a printout nor in a log.
func TestCommandArgsNeverLeakCode(t *testing.T) {
	const secret = "482915"
	args := CommandArgs{Code: secret, Force: true}
	data := AlarmCommandData{Command: CmdDisarm, Args: args, Origin: OriginApp}

	for name, out := range map[string]string{
		"%v":                 fmt.Sprintf("%v", args),
		"%s":                 fmt.Sprintf("%s", args),
		"%+v":                fmt.Sprintf("%+v", args),
		"%#v":                fmt.Sprintf("%#v", args),
		"%v on the command":  fmt.Sprintf("%v", data),
		"%+v on the command": fmt.Sprintf("%+v", data),
	} {
		if strings.Contains(out, secret) {
			t.Errorf("the code leaks with %s: %s", name, out)
		}
	}

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("order received", "args", args)
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("order received", "args", args)
	if strings.Contains(buf.String(), secret) {
		t.Errorf("the code leaks into the log: %s", buf.String())
	}

	// But it must travel in the message itself.
	b, _ := json.Marshal(data)
	if !strings.Contains(string(b), secret) {
		t.Error("the code must be present in the order's JSON")
	}
}

func TestAckNeverContainsCodeField(t *testing.T) {
	b, _ := json.Marshal(AlarmAckData{Ref: "x", Status: AckRefused, Reason: ReasonBadCode})
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, has := m["code"]; has {
		t.Errorf("an acknowledgement must have no code field: %s", b)
	}
}

func TestAlarmEventsMarshalShape(t *testing.T) {
	ends := time.Date(2026, 10, 17, 21, 6, 5, 0, time.UTC)
	b, err := json.Marshal(ArmingData{Origin: OriginKeypad, User: "u_42", EndsAt: TS(ends), Bypassed: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["ends_at"] != "2026-10-17T21:06:05.000Z" || m["origin"] != "keypad" || m["forced"] != false {
		t.Fatalf("unexpected shape: %s", b)
	}
	if _, ok := m["bypassed"].([]any); !ok {
		t.Fatalf("bypassed must stay a list, even when empty: %s", b)
	}
}
