package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"
)

func seededEntropy() *rand.Rand { return rand.New(rand.NewSource(1)) }

func TestIDFormatAndOrder(t *testing.T) {
	now := time.Date(2026, 10, 17, 21, 4, 5, 0, time.UTC)
	g := NewIDGeneratorWith(func() time.Time { return now }, seededEntropy())

	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 1000; i++ {
		// 100 identifiers per millisecond, so ties are exercised.
		if i%100 == 0 {
			now = now.Add(time.Millisecond)
		}
		id := g.New()
		if !ValidID(id) {
			t.Fatalf("invalid identifier: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate: %q", id)
		}
		seen[id] = true
		if id <= prev {
			t.Fatalf("not increasing: %q after %q", id, prev)
		}
		prev = id
	}
}

func TestIDGeneratorRealClock(t *testing.T) {
	g := NewIDGenerator()
	a, b := g.New(), g.New()
	if !ValidID(a) || !ValidID(b) || a >= b {
		t.Fatalf("want two valid increasing identifiers, got %q %q", a, b)
	}
}

func TestTimestampJSON(t *testing.T) {
	in := time.Date(2026, 10, 17, 23, 4, 5, 312_000_000, time.FixedZone("UTC+2", 2*3600))
	b, err := json.Marshal(TS(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"2026-10-17T21:04:05.312Z"` {
		t.Fatalf("unexpected format: %s", b)
	}
	var out Timestamp
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Equal(in) {
		t.Fatalf("round trip: %v != %v", out.Time, in)
	}
	if err := json.Unmarshal([]byte(`"not a date"`), &out); err == nil {
		t.Fatal("an invalid date must be refused")
	}
}

func newTestEnvelope(t *testing.T, data any) Envelope {
	t.Helper()
	id := NewIDGeneratorWith(func() time.Time { return time.Unix(1_700_000_000, 0) }, seededEntropy()).New()
	e, err := NewEnvelope("tg-lom-0001", "adapter.zigbee", 48213, id,
		time.Date(2026, 10, 17, 21, 4, 5, 312_000_000, time.UTC), TypeDeviceEvent, data)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEnvelopeRoundTrip(t *testing.T) {
	want := DeviceEventData{DeviceID: "dev_k7p2", Capability: CapContact, Value: json.RawMessage(`"open"`)}
	e := newTestEnvelope(t, want)

	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseEnvelope(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Site != "tg-lom-0001" || got.Seq != 48213 || got.V != 1 || got.Type != TypeDeviceEvent {
		t.Fatalf("envelope read wrongly: %+v", got)
	}
	data, err := DecodeData[DeviceEventData](got)
	if err != nil {
		t.Fatal(err)
	}
	if data.DeviceID != want.DeviceID || data.StringValue() != "open" {
		t.Fatalf("data read wrongly: %+v", data)
	}
	if err := data.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEnvelopeIgnoresUnknownFields(t *testing.T) {
	e := newTestEnvelope(t, map[string]any{"device_id": "d", "capability": "contact", "value": "open", "future": 1})
	b, _ := json.Marshal(e)
	b = bytes.Replace(b, []byte(`"v":1`), []byte(`"v":1,"future_field":true`), 1)
	got, err := ParseEnvelope(b)
	if err != nil {
		t.Fatalf("an unknown field must not break anything: %v", err)
	}
	if _, err := DecodeData[DeviceEventData](got); err != nil {
		t.Fatal(err)
	}
}

func TestEnvelopeRejects(t *testing.T) {
	good := newTestEnvelope(t, map[string]any{})
	cases := map[string]func(*Envelope){
		"future version":  func(e *Envelope) { e.V = Version + 1 },
		"zero version":    func(e *Envelope) { e.V = 0 },
		"malformed id":    func(e *Envelope) { e.ID = "not-a-ulid" },
		"empty site":      func(e *Envelope) { e.Site = "" },
		"uppercase site":  func(e *Envelope) { e.Site = "TG-LOM" },
		"empty src":       func(e *Envelope) { e.Src = "" },
		"empty type":      func(e *Envelope) { e.Type = "" },
		"missing ts":      func(e *Envelope) { e.TS = Timestamp{} },
		"missing data":    func(e *Envelope) { e.Data = nil },
		"data not object": func(e *Envelope) { e.Data = json.RawMessage(`"text"`) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := good
			mutate(&e)
			err := e.Validate()
			if err == nil {
				t.Fatal("want an error")
			}
			if name == "future version" && !errors.Is(err, ErrUnsupportedVersion) {
				t.Fatalf("want ErrUnsupportedVersion, got %v", err)
			}
			if name != "future version" && !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
	if _, err := ParseEnvelope([]byte(`{not json`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("broken JSON must give ErrInvalid, got %v", err)
	}
}

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
	if !KnownAlarmCommand(CmdPostpone) || KnownAlarmCommand("alarm.explode") {
		t.Error("wrong list of known commands")
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

func TestTopics(t *testing.T) {
	cases := map[string]string{
		DeviceEventTopic("tg-lom-0001", "dev_k7p2"):             "safehome/tg-lom-0001/device/dev_k7p2/event",
		DeviceCmdTopic("tg-lom-0001", "dev_s3x9"):               "safehome/tg-lom-0001/device/dev_s3x9/cmd",
		AlarmCmdTopic("tg-lom-0001"):                            "safehome/tg-lom-0001/alarm/cmd",
		AlarmStateTopic("tg-lom-0001"):                          "safehome/tg-lom-0001/alarm/state",
		DeviceStateTopic("tg-lom-0001", "dev_k7p2", CapBattery): "safehome/tg-lom-0001/device/dev_k7p2/state/battery",
		AlertRequestTopic("tg-lom-0001"):                        "safehome/tg-lom-0001/alert/request",
		HeartbeatTopic("tg-lom-0001"):                           "safehome/tg-lom-0001/system/heartbeat",
		AgentCmdTopic("tg-lom-0001"):                            "safehome/tg-lom-0001/agent/cmd",
		AgentReplayedTopic("tg-lom-0001"):                       "safehome/tg-lom-0001/agent/replayed",
		InstallModeTopic("tg-lom-0001"):                         "safehome/tg-lom-0001/install/mode",
		InstallCmdTopic("tg-lom-0001"):                          "safehome/tg-lom-0001/install/cmd",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("topic %q, want %q", got, want)
		}
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

func TestParseTopic(t *testing.T) {
	good := map[string]struct {
		site  string
		parts []string
	}{
		"safehome/tg-lom-0001/alarm/cmd":             {"tg-lom-0001", []string{"alarm", "cmd"}},
		"safehome/tg-lom-0001/device/dev_k7p2/event": {"tg-lom-0001", []string{"device", "dev_k7p2", "event"}},
		"safehome/tg-lom-0001/system/health":         {"tg-lom-0001", []string{"system", "health"}},
	}
	for topic, want := range good {
		site, parts, ok := ParseTopic(topic)
		if !ok || site != want.site || !slices.Equal(parts, want.parts) {
			t.Errorf("%s: %q %v %v", topic, site, parts, ok)
		}
	}
	for _, topic := range []string{"", "safehome", "safehome/", "safehome/tg-lom-0001", "safehome/tg-lom-0001/", "safehome/TG/alarm/cmd",
		"other/tg-lom-0001/alarm/cmd", "safehome/tg-lom-0001//cmd", "safehome//alarm/cmd", "safehome/tg-lom-0001/alarm/", "/safehome/tg-lom-0001/alarm/cmd"} {
		if _, _, ok := ParseTopic(topic); ok {
			t.Errorf("%q must be refused", topic)
		}
	}
	site, parts, ok := ParseTopic(DeviceCmdTopic("tg-lom-0001", "dev_s3x9"))
	if !ok || site != "tg-lom-0001" || !slices.Equal(parts, []string{"device", "dev_s3x9", "cmd"}) {
		t.Fatalf("%q %v %v", site, parts, ok)
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

func TestRegistryAndZigbeeTopics(t *testing.T) {
	for got, want := range map[string]string{
		RegistryCmdTopic("s"):     "safehome/s/registry/cmd",
		RegistryEventTopic("s"):   "safehome/s/registry/event",
		RegistryDevicesTopic("s"): "safehome/s/registry/devices",
		ZigbeeStatusTopic("s"):    "safehome/s/zigbee/status",
	} {
		if got != want {
			t.Errorf("%s, want %s", got, want)
		}
	}
}

func TestVideoRequestValidate(t *testing.T) {
	exp := time.Date(2026, 10, 17, 21, 6, 5, 0, time.UTC)
	if err := goodVideoRequest(exp).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*VideoRequestData){
		"no session":      func(d *VideoRequestData) { d.Session = "" },
		"no camera":       func(d *VideoRequestData) { d.Camera = "" },
		"bad reason":      func(d *VideoRequestData) { d.Reason = "curious" },
		"negative length": func(d *VideoRequestData) { d.MaxS = -1 },
		"too long":        func(d *VideoRequestData) { d.MaxS = MaxVideoLength + 1 },
		"no token":        func(d *VideoRequestData) { d.Token = "" },
		"no exp":          func(d *VideoRequestData) { d.Exp = Timestamp{} },
		"plain rtmp":      func(d *VideoRequestData) { d.PushURL = "rtmp://relay.example/x" },
		"https":           func(d *VideoRequestData) { d.PushURL = "https://relay.example/x" },
		"no host":         func(d *VideoRequestData) { d.PushURL = "rtmps:///x" },
		"credentials":     func(d *VideoRequestData) { d.PushURL = "rtmps://user:pw@relay.example/x" },
		"query":           func(d *VideoRequestData) { d.PushURL = "rtmps://relay.example/x?token=t" },
		"not a URL":       func(d *VideoRequestData) { d.PushURL = "://" },
	} {
		d := goodVideoRequest(exp)
		mutate(&d)
		err := d.Validate()
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		} else if strings.Contains(err.Error(), "tok-secret-123") {
			t.Errorf("%s: the error leaks the token", name)
		}
	}
	unlimited := goodVideoRequest(exp)
	unlimited.MaxS = 0
	if err := unlimited.Validate(); err != nil {
		t.Fatalf("no limit on a fixed line: %v", err)
	}
	if (VideoStopData{Session: "s"}).Validate() != nil {
		t.Fatal("a session is enough")
	}
	if err := (VideoStopData{}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestTheVideoTokenIsNeverPrinted(t *testing.T) {
	d := goodVideoRequest(time.Date(2026, 10, 17, 21, 6, 5, 0, time.UTC))
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("request", "data", d)
	for _, out := range []string{fmt.Sprint(d), fmt.Sprintf("%v", d), fmt.Sprintf("%+v", d), fmt.Sprintf("%#v", d), logs.String()} {
		if strings.Contains(out, "tok-secret-123") || !strings.Contains(out, "cam_caisse") {
			t.Errorf("%q", out)
		}
	}
	// The JSON sent on the wire still carries it: the site needs it.
	raw, _ := json.Marshal(d)
	if !strings.Contains(string(raw), "tok-secret-123") {
		t.Fatal("the token is part of the message")
	}
}

func TestVideoTopics(t *testing.T) {
	for got, want := range map[string]string{
		VideoCmdTopic("s"):     "safehome/s/video/cmd",
		VideoSessionTopic("s"): "safehome/s/video/session",
		VideoStatusTopic("s"):  "safehome/s/video/status",
	} {
		if got != want {
			t.Errorf("%s, want %s", got, want)
		}
	}
}
