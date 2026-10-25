package z2m

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"safehome/internal/zigbee"
)

var now = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

func parse(t *testing.T, topic, payload string, retained bool) (zigbee.Input, bool) {
	t.Helper()
	in, ok, err := Parse(DefaultBase, topic, []byte(payload), retained, now)
	if err != nil {
		t.Fatalf("%s %s: %v", topic, payload, err)
	}
	return in, ok
}

func TestDeviceMessagesBecomeReports(t *testing.T) {
	in, ok := parse(t, "zigbee2mqtt/dev_k7p2", `{"contact":false,"battery":91,"linkquality":120,"voltage":3005}`, false)
	want := zigbee.Report{Now: now, Name: "dev_k7p2", Fields: map[string]any{"contact": false, "battery": 91.0, "linkquality": 120.0, "voltage": 3005.0}}
	if !ok || !reflect.DeepEqual(in, want) {
		t.Fatalf("%+v", in)
	}
	in, _ = parse(t, "zigbee2mqtt/0x00158d0001000001", `{"contact":true}`, true)
	if r := in.(zigbee.Report); !r.Retained || r.Name != "0x00158d0001000001" {
		t.Fatalf("%+v", r)
	}
}

func TestTheBridgeStateInBothFormats(t *testing.T) {
	for payload, online := range map[string]bool{
		`online`: true, `offline`: false, `{"state":"online"}`: true, `{"state":"offline"}`: false,
	} {
		in, ok := parse(t, "zigbee2mqtt/bridge/state", payload, true)
		if !ok || in != (zigbee.Bridge{Now: now, Online: online}) {
			t.Errorf("%s: %+v", payload, in)
		}
	}
}

func TestInterviewsAndDeparturesFromTheBridge(t *testing.T) {
	cases := map[string]zigbee.Input{
		`{"type":"device_interview","data":{"friendly_name":"0x01","ieee_address":"0x01","status":"successful","supported":true,"definition":{"model":"MCCGQ11LM","vendor":"Aqara"}}}`: zigbee.Interviewed{Now: now, IEEE: "0x01", Model: "MCCGQ11LM", OK: true},
		`{"type":"device_interview","data":{"ieee_address":"0x02","status":"successful","supported":false,"definition":null}}`:                                                         zigbee.Interviewed{Now: now, IEEE: "0x02", OK: true},
		`{"type":"device_interview","data":{"ieee_address":"0x03","status":"failed"}}`:                                                                                                 zigbee.Interviewed{Now: now, IEEE: "0x03"},
		`{"type":"device_leave","data":{"ieee_address":"0x04","friendly_name":"dev_x"}}`:                                                                                               zigbee.Left{Now: now, IEEE: "0x04"},
		`{"type":"device_joined","data":{"ieee_address":"0x05","friendly_name":"0x05"}}`:                                                                                               zigbee.Joined{Now: now, IEEE: "0x05"},
	}
	for payload, want := range cases {
		in, ok := parse(t, "zigbee2mqtt/bridge/event", payload, false)
		if !ok || !reflect.DeepEqual(in, want) {
			t.Errorf("%s: %+v", payload, in)
		}
	}
	for _, payload := range []string{
		`{"type":"device_interview","data":{"ieee_address":"0x01","status":"started"}}`,
		`{"type":"device_announce","data":{"ieee_address":"0x01"}}`,
	} {
		if _, ok := parse(t, "zigbee2mqtt/bridge/event", payload, false); ok {
			t.Errorf("%s: the core waits for the end of the interview", payload)
		}
	}
	if _, _, err := Parse(DefaultBase, "zigbee2mqtt/bridge/event", []byte(`{"type":"device_joined","data":{}}`), false, now); err == nil {
		t.Error("a device that joined without an address")
	}
	for payload, want := range map[zigbee.Action]string{
		zigbee.Expel{IEEE: "dev_x"}:              `{"id":"dev_x"}`,
		zigbee.Expel{IEEE: "dev_x", Force: true}: `{"force":true,"id":"dev_x"}`,
	} {
		if msg, ok, err := Build(DefaultBase, payload); err != nil || !ok || string(msg.Payload) != want || msg.Topic != "zigbee2mqtt/bridge/request/device/remove" {
			t.Errorf("%+v: %s %s", payload, msg.Topic, msg.Payload)
		}
	}
}

func TestWhatTheCoreDoesNotNeedIsLeftOut(t *testing.T) {
	for _, topic := range []string{
		"zigbee2mqtt/bridge/devices", "zigbee2mqtt/bridge/info", "zigbee2mqtt/bridge/logging",
		"zigbee2mqtt/bridge/response/device/rename", "zigbee2mqtt/bridge/request/permit_join",
		"zigbee2mqtt/dev_x/availability", "zigbee2mqtt/dev_x/set", "zigbee2mqtt/bridge",
		"zigbee2mqtt/", "zigbee2mqtt", "other/dev_x", "zigbee2mqttx/dev_x",
	} {
		if _, ok := parse(t, topic, `{"state":"online"}`, false); ok {
			t.Errorf("%s: kept", topic)
		}
	}
	if _, ok := parse(t, "zigbee2mqtt/dev_x", ``, true); ok {
		t.Error("an empty retained message clears a topic: nothing to read")
	}
}

func TestBrokenMessagesAreErrors(t *testing.T) {
	for topic, payload := range map[string]string{
		"zigbee2mqtt/dev_x":        `[1,2]`,
		"zigbee2mqtt/dev_y":        `{`,
		"zigbee2mqtt/dev_z":        `null`,
		"zigbee2mqtt/bridge/state": `{"state":`,
		"zigbee2mqtt/bridge/event": `nope`,
	} {
		if _, _, err := Parse(DefaultBase, topic, []byte(payload), false, now); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s %s: %v", topic, payload, err)
		}
	}
	if _, _, err := Parse(DefaultBase, "zigbee2mqtt/bridge/state", []byte("starting"), false, now); !errors.Is(err, ErrInvalid) {
		t.Error("an unknown bridge state")
	}
	if _, _, err := Parse(DefaultBase, "zigbee2mqtt/bridge/event", []byte(`{"type":"device_leave","data":{}}`), false, now); !errors.Is(err, ErrInvalid) {
		t.Error("an event without an address")
	}
}

func TestAnotherBaseTopic(t *testing.T) {
	in, ok, err := Parse("z2m-shop", "z2m-shop/dev_x", []byte(`{"contact":true}`), false, now)
	if err != nil || !ok || in.(zigbee.Report).Name != "dev_x" {
		t.Fatalf("%+v %v", in, err)
	}
}

func TestOnlyWhatTheCoreReadsIsSubscribedTo(t *testing.T) {
	filters := Filters("z2m-shop")
	matches := func(topic string) bool {
		return slices.ContainsFunc(filters, func(f string) bool { return topicMatches(f, topic) })
	}
	for _, topic := range []string{"z2m-shop/dev_x", "z2m-shop/0x00158d0001000001", "z2m-shop/bridge/state",
		"z2m-shop/bridge/event", "z2m-shop/bridge/response/permit_join", "z2m-shop/bridge/response/device/rename"} {
		if !matches(topic) {
			t.Errorf("%s: not received", topic)
		}
	}
	// Large retained messages the core has no use for (bridge/definitions was 228 kB
	// on the trial's Zigbee2MQTT 2.14.2, over the adapter's limit).
	for _, topic := range []string{"z2m-shop/bridge/definitions", "z2m-shop/bridge/devices", "z2m-shop/bridge/info",
		"z2m-shop/bridge/logging", "z2m-shop/dev_x/availability", "z2m-shop/dev_x/set", "zigbee2mqtt/dev_x"} {
		if matches(topic) {
			t.Errorf("%s: received for nothing", topic)
		}
	}
}

// topicMatches is MQTT's filter matching, enough for these filters.
func topicMatches(filter, topic string) bool {
	f, tp := strings.Split(filter, "/"), strings.Split(topic, "/")
	for i, part := range f {
		switch {
		case part == "#":
			return true
		case i >= len(tp):
			return false
		case part != "+" && part != tp[i]:
			return false
		}
	}
	return len(f) == len(tp)
}

// Zigbee2MQTT 2.14.2 on the trial: asked for 300 s, it refused. Zigbee2MQTT 2.x
// echoes the request's transaction in its answer, an error or not.
func TestAnswersToOpenTheNetworkReachTheCoreWithTheirTransaction(t *testing.T) {
	const topic = "zigbee2mqtt/bridge/response/permit_join"
	for payload, want := range map[string]zigbee.JoinAnswer{
		`{"data":{},"status":"error","error":"Cannot permit join for more than 254 seconds.","transaction":"p1.1"}`: {
			Now: now, Txn: "p1.1", Reason: "Cannot permit join for more than 254 seconds."},
		`{"data":{"time":254},"status":"ok","transaction":"p1.2"}`: {Now: now, Txn: "p1.2", OK: true},
		`{"data":{"time":0},"status":"ok"}`:                        {Now: now, OK: true}, // a closing order
		`{"data":{"time":254},"status":"ok","transaction":23}`:     {Now: now, OK: true}, // not one of ours
		`{"data":{},"status":"error","error":"Invalid payload"}`:   {Now: now, Reason: "Invalid payload"},
	} {
		if in, ok := parse(t, topic, payload, false); !ok || in != want {
			t.Errorf("%s: %+v", payload, in)
		}
	}
	for _, payload := range []string{`nope`, `{"status":"maybe"}`} {
		if _, _, err := Parse(DefaultBase, topic, []byte(payload), false, now); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", payload, err)
		}
	}
}

func TestErrorsReportedByTheBridge(t *testing.T) {
	req, reason, failed := ResponseError(DefaultBase, "zigbee2mqtt/bridge/response/device/rename",
		[]byte(`{"data":{},"status":"error","error":"Device '0x01' does not exist"}`))
	if !failed || req != "device/rename" || reason != "Device '0x01' does not exist" {
		t.Fatalf("%q %q %v", req, reason, failed)
	}
	for topic, payload := range map[string]string{
		"zigbee2mqtt/bridge/response/permit_join": `{"data":{"value":true},"status":"ok"}`,
		"zigbee2mqtt/bridge/response/x":           `nope`,
		"zigbee2mqtt/bridge/event":                `{"status":"error"}`,
	} {
		if _, _, failed := ResponseError(DefaultBase, topic, []byte(payload)); failed {
			t.Errorf("%s: not an error", topic)
		}
	}
}

func build(t *testing.T, a zigbee.Action) (string, map[string]any) {
	t.Helper()
	m, ok, err := Build(DefaultBase, a)
	if err != nil || !ok {
		t.Fatalf("%+v: %v", a, err)
	}
	var body map[string]any
	if err := json.Unmarshal(m.Payload, &body); err != nil {
		t.Fatal(err)
	}
	return m.Topic, body
}

func TestActionsBecomeRequests(t *testing.T) {
	topic, body := build(t, zigbee.Send{IEEE: "0x02", Payload: map[string]any{"warning": map[string]any{"mode": "burglar", "duration": 300}}})
	if topic != "zigbee2mqtt/0x02/set" || body["warning"].(map[string]any)["duration"] != 300.0 {
		t.Fatalf("%s %v", topic, body)
	}
	topic, body = build(t, zigbee.PermitJoin{Seconds: 120, Txn: "p1.1"})
	if topic != "zigbee2mqtt/bridge/request/permit_join" || body["value"] != true || body["time"] != 120.0 || body["transaction"] != "p1.1" {
		t.Fatalf("%s %v", topic, body)
	}
	_, body = build(t, zigbee.PermitJoin{})
	if _, txn := body["transaction"]; body["value"] != false || body["time"] != 0.0 || txn {
		t.Fatalf("closing: %v", body)
	}
	topic, body = build(t, zigbee.CarriesCode{IEEE: "0x04"})
	if topic != "zigbee2mqtt/bridge/request/device/options" || body["id"] != "0x04" || !reflect.DeepEqual(body["options"], map[string]any{"qos": 0.0}) {
		t.Fatalf("%s %v", topic, body)
	}
	topic, body = build(t, zigbee.Rename{IEEE: "0x01", To: "dev_k7p2"})
	if topic != "zigbee2mqtt/bridge/request/device/rename" || body["from"] != "0x01" || body["to"] != "dev_k7p2" {
		t.Fatalf("%s %v", topic, body)
	}
	topic, body = build(t, zigbee.Expel{IEEE: "0x03"})
	if topic != "zigbee2mqtt/bridge/request/device/remove" || body["id"] != "0x03" {
		t.Fatalf("%s %v", topic, body)
	}
	if _, ok, _ := Build(DefaultBase, zigbee.Ack{}); ok {
		t.Fatal("an acknowledgement is for the contract, not for Zigbee2MQTT")
	}
	if _, _, err := Build(DefaultBase, zigbee.Send{IEEE: "0x02", Payload: map[string]any{"x": math.NaN()}}); err == nil {
		t.Fatal("a payload that cannot be encoded")
	}
}

// last_seen (advanced.last_seen) says when Zigbee2MQTT heard the device, in either of
// its formats; a message may reach us long after.
func TestLastSeenSaysWhenTheDeviceWasHeard(t *testing.T) {
	heard := now.Add(-2 * time.Hour)
	for payload, want := range map[string]time.Time{
		`{"action":"disarm","last_seen":"` + heard.Format(time.RFC3339Nano) + `"}`:                          heard,
		`{"action":"disarm","last_seen":"` + heard.In(time.FixedZone("", 3600)).Format(time.RFC3339) + `"}`: heard,
		`{"action":"disarm","last_seen":` + fmt.Sprint(heard.UnixMilli()) + `}`:                             heard,
		`{"action":"disarm"}`:                       {},
		`{"action":"disarm","last_seen":"later"}`:   {},
		`{"action":"disarm","last_seen":"disable"}`: {},
	} {
		in, _ := parse(t, "zigbee2mqtt/dev_kp", payload, false)
		if got := in.(zigbee.Report).Heard; !got.Equal(want) {
			t.Errorf("%s: heard %v, want %v", payload, got, want)
		}
	}
}
