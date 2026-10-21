package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestDeviceEventValidate(t *testing.T) {
	ok := []DeviceEventData{
		{DeviceID: "d", Capability: CapContact, Value: json.RawMessage(`"open"`)},
		{DeviceID: "d", Capability: CapMotion, Value: json.RawMessage(`"clear"`)},
		{DeviceID: "d", Capability: CapBattery, Value: json.RawMessage(`82`)},
		{DeviceID: "d", Capability: CapLink, Value: json.RawMessage(`0`)},
		{DeviceID: "d", Capability: CapKeypad, Value: json.RawMessage(`"disarm"`)},
	}
	for _, d := range ok {
		if err := d.Validate(); err != nil {
			t.Errorf("%s=%s should be valid: %v", d.Capability, d.Value, err)
		}
	}
	bad := []DeviceEventData{
		{DeviceID: "", Capability: CapContact, Value: json.RawMessage(`"open"`)},
		{DeviceID: "d", Capability: "unknown", Value: json.RawMessage(`"x"`)},
		{DeviceID: "d", Capability: CapContact, Value: json.RawMessage(`"ajar"`)},
		{DeviceID: "d", Capability: CapContact, Value: json.RawMessage(`1`)},
		{DeviceID: "d", Capability: CapBattery, Value: json.RawMessage(`101`)},
		{DeviceID: "d", Capability: CapBattery, Value: json.RawMessage(`-1`)},
		{DeviceID: "d", Capability: CapBattery, Value: json.RawMessage(`"full"`)},
		{DeviceID: "d", Capability: CapPanic, Value: json.RawMessage(`"released"`)},
	}
	for _, d := range bad {
		if err := d.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s=%s should be refused, got %v", d.Capability, d.Value, err)
		}
	}
}

func TestIsActive(t *testing.T) {
	active := [][2]string{
		{CapContact, ValOpen}, {CapMotion, ValDetected}, {CapSmoke, ValAlarm},
		{CapVibration, ValDetected}, {CapGlassBreak, ValDetected},
		{CapPanic, ValPressed}, {CapTamper, ValTriggered},
	}
	for _, a := range active {
		if !IsActive(a[0], a[1]) {
			t.Errorf("%s=%s should be an activation", a[0], a[1])
		}
	}
	inactive := [][2]string{
		{CapContact, ValClosed}, {CapMotion, ValClear}, {CapSmoke, ValClear},
		{CapTamper, ValClear}, {CapBattery, "10"}, {CapSiren, ValOn}, {"unknown", ValOpen},
	}
	for _, a := range inactive {
		if IsActive(a[0], a[1]) {
			t.Errorf("%s=%s should not be an activation", a[0], a[1])
		}
	}
}

func TestKeypadCodeValidation(t *testing.T) {
	ok := []DeviceEventData{
		{DeviceID: "kp", Capability: CapKeypad, Value: json.RawMessage(`"disarm"`), Code: "482915"},
		{DeviceID: "kp", Capability: CapKeypad, Value: json.RawMessage(`"silence"`), Code: "4829"},
		{DeviceID: "kp", Capability: CapKeypad, Value: json.RawMessage(`"disarm"`)}, // no code: the engine treats it as a wrong one
		{DeviceID: "kp", Capability: CapKeypad, Value: json.RawMessage(`"arm"`)},
	}
	for _, d := range ok {
		if err := d.Validate(); err != nil {
			t.Errorf("%v: %v", d, err)
		}
	}
	bad := []DeviceEventData{
		{DeviceID: "kp", Capability: CapKeypad, Value: json.RawMessage(`"arm"`), Code: "482915"},
		{DeviceID: "d", Capability: CapContact, Value: json.RawMessage(`"open"`), Code: "482915"},
		{DeviceID: "kp", Capability: CapKeypad, Value: json.RawMessage(`"disarm"`), Code: "48a915"},
		{DeviceID: "kp", Capability: CapKeypad, Value: json.RawMessage(`"disarm"`), Code: "12345678901234567"},
	}
	for _, d := range bad {
		err := d.Validate()
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: want ErrInvalid, got %v", d, err)
		} else if d.Code != "" && strings.Contains(err.Error(), d.Code) {
			t.Errorf("the error repeats the code: %v", err)
		}
	}
}

// A keypad code must appear neither in a printout nor in a log.
func TestDeviceEventNeverLeaksCode(t *testing.T) {
	const secret = "482915"
	d := DeviceEventData{DeviceID: "kp", Capability: CapKeypad, Value: json.RawMessage(`"disarm"`), Code: secret}
	for name, out := range map[string]string{
		"%v": fmt.Sprintf("%v", d), "%+v": fmt.Sprintf("%+v", d), "%#v": fmt.Sprintf("%#v", d), "%s": fmt.Sprintf("%s", d),
		"pointer %v": fmt.Sprintf("%v", &d),
	} {
		if strings.Contains(out, secret) {
			t.Errorf("the code leaks with %s: %s", name, out)
		}
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("keypad", "event", d)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("keypad", "event", d)
	if strings.Contains(buf.String(), secret) {
		t.Errorf("the code leaks into the log: %s", buf.String())
	}
	b, err := json.Marshal(d)
	if err != nil || !strings.Contains(string(b), secret) {
		t.Error("the code must be in the message itself, or the hub could not check it")
	}
}

func TestDeviceCommandValidate(t *testing.T) {
	ok := []DeviceCommandData{
		{DeviceID: "s", Command: CmdSirenOn, Args: DeviceCommandArgs{DurationS: 300}},
		{DeviceID: "s", Command: CmdSirenOff},
		{DeviceID: "s", Command: CmdDeviceIdentify},
	}
	for _, d := range ok {
		if err := d.Validate(); err != nil {
			t.Errorf("%+v: %v", d, err)
		}
	}
	bad := []DeviceCommandData{
		{Command: CmdSirenOff},
		{DeviceID: "s", Command: "door.unlock"},
		{DeviceID: "s", Command: CmdSirenOn},
	}
	for _, d := range bad {
		if err := d.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: want ErrInvalid, got %v", d, err)
		}
	}
}

func TestRegistryOrdersValidate(t *testing.T) {
	if (RegistryPairData{DurationS: 0}).Validate() != nil || (RegistryPairData{DurationS: 120}).Validate() != nil {
		t.Fatal("zero closes the network, a positive duration opens it")
	}
	if err := (RegistryPairData{DurationS: -1}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if (RegistryRemoveData{DeviceID: "d"}).Validate() != nil {
		t.Fatal("a device id is enough")
	}
	if err := (RegistryRemoveData{}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
