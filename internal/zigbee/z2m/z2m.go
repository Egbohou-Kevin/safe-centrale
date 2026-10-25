// Package z2m translates between Zigbee2MQTT's messages and the zigbee core: it is
// the only package that knows Zigbee2MQTT's topics and payloads. Replacing
// Zigbee2MQTT by another bridge means rewriting this package, not the core.
//
// The topics and fields come from Zigbee2MQTT's documentation (versions 1.x and 2.x)
// and must be checked against the installed version (docs/adapter-zigbee.md).
//
// It does no I/O: the application hands it what the broker delivered and publishes
// what it returns.
package z2m

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"safehome/internal/zigbee"
)

// DefaultBase is Zigbee2MQTT's default base topic.
const DefaultBase = "zigbee2mqtt"

// Filters are the subscriptions that receive what the core needs from Zigbee2MQTT:
// the devices' messages (one level under the base: a device is named by our id or
// its address, never with a slash), the bridge's state and events, and its answers
// to our requests. The rest of bridge/ is left on the broker: bridge/definitions
// (every model Zigbee2MQTT knows, more than 200 kB, retained) and bridge/devices
// (which grows with the network) would only be read to be thrown away.
func Filters(base string) []string {
	return []string{base + "/+", base + "/bridge/state", base + "/bridge/event", base + "/bridge/response/#"}
}

// Message is a message to publish to Zigbee2MQTT.
type Message struct {
	Topic   string
	Payload []byte
}

// ErrInvalid wraps a message that cannot be read.
var ErrInvalid = errors.New("z2m: invalid message")

// Parse turns a message from Zigbee2MQTT into an input for the core. ok is false for
// a message the core does not need (other bridge topics, a device's availability,
// requests going to Zigbee2MQTT).
func Parse(base, topic string, payload []byte, retained bool, now time.Time) (in zigbee.Input, ok bool, err error) {
	rest, found := strings.CutPrefix(topic, base+"/")
	if !found || rest == "" {
		return nil, false, nil
	}
	if bridge, isBridge := strings.CutPrefix(rest, "bridge/"); isBridge || rest == "bridge" {
		switch bridge {
		case "state":
			return parseState(payload, retained, now)
		case "event":
			return parseEvent(payload, now)
		case "response/permit_join":
			return parsePermitJoin(payload, now)
		case "response/health_check":
			return parseHealth(payload, now)
		}
		return nil, false, nil
	}
	if strings.Contains(rest, "/") {
		return nil, false, nil // name/availability, name/set, name/get...
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, false, nil // a retained message being cleared
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return nil, false, fmt.Errorf("%w: the message of a device is not a JSON object", ErrInvalid)
	}
	return zigbee.Report{Now: now, Name: rest, Fields: fields, Retained: retained, Heard: lastSeen(fields)}, true, nil
}

// lastSeen reads when Zigbee2MQTT heard the device: the last_seen field it adds to
// every message of a device with advanced.last_seen set (deploy/zigbee2mqtt), an ISO
// 8601 time or milliseconds since the epoch. Zero when the field is absent or
// unreadable.
func lastSeen(fields map[string]any) time.Time {
	switch v := fields["last_seen"].(type) {
	case string:
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t
		}
	case float64:
		if v > 0 {
			return time.UnixMilli(int64(v))
		}
	}
	return time.Time{}
}

// parseState reads bridge/state: "online" or {"state": "online"} depending on the
// version. Zigbee2MQTT publishes it retained: the copy the broker hands over at the
// start may date from before a power cut (see zigbee.Bridge).
func parseState(payload []byte, retained bool, now time.Time) (zigbee.Input, bool, error) {
	state := strings.TrimSpace(string(payload))
	if strings.HasPrefix(state, "{") {
		var v struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(payload, &v); err != nil {
			return nil, false, fmt.Errorf("%w: bridge state: %v", ErrInvalid, err)
		}
		state = v.State
	}
	switch state {
	case "online":
		return zigbee.Bridge{Now: now, Online: true, Retained: retained}, true, nil
	case "offline":
		return zigbee.Bridge{Now: now, Retained: retained}, true, nil
	}
	return nil, false, fmt.Errorf("%w: unknown bridge state %q", ErrInvalid, state)
}

type bridgeEvent struct {
	Type string `json:"type"`
	Data struct {
		IEEE       string `json:"ieee_address"`
		Status     string `json:"status"`
		Definition *struct {
			Model string `json:"model"`
		} `json:"definition"`
	} `json:"data"`
}

// parseEvent reads bridge/event: the end of a new device's interview, and a device
// leaving. Zigbee2MQTT gives no definition for a model it does not support: the
// model is then empty, and the core refuses the device.
func parseEvent(payload []byte, now time.Time) (zigbee.Input, bool, error) {
	var e bridgeEvent
	if err := json.Unmarshal(payload, &e); err != nil {
		return nil, false, fmt.Errorf("%w: bridge event: %v", ErrInvalid, err)
	}
	if e.Data.IEEE == "" && (e.Type == "device_interview" || e.Type == "device_leave" || e.Type == "device_joined") {
		return nil, false, fmt.Errorf("%w: %s without an address", ErrInvalid, e.Type)
	}
	switch e.Type {
	case "device_interview":
		switch e.Data.Status {
		case "successful":
			in := zigbee.Interviewed{Now: now, IEEE: e.Data.IEEE, OK: true}
			if e.Data.Definition != nil {
				in.Model = e.Data.Definition.Model
			}
			return in, true, nil
		case "failed":
			return zigbee.Interviewed{Now: now, IEEE: e.Data.IEEE}, true, nil
		}
	case "device_leave":
		return zigbee.Left{Now: now, IEEE: e.Data.IEEE}, true, nil
	case "device_joined":
		return zigbee.Joined{Now: now, IEEE: e.Data.IEEE}, true, nil
	}
	return nil, false, nil // device_announce, interview started...
}

// parsePermitJoin reads Zigbee2MQTT's answer to a request to open or close the
// network: {"data":{"time":254},"status":"ok","transaction":"..."}, or with
// "status":"error" and "error". Zigbee2MQTT (2.x, lib/util/utils.ts getResponse)
// echoes the request's transaction in both; the core matches it to its request. A
// transaction that is not a string is not one of ours: it is left empty.
func parsePermitJoin(payload []byte, now time.Time) (zigbee.Input, bool, error) {
	var r struct {
		Status      string `json:"status"`
		Error       string `json:"error"`
		Transaction any    `json:"transaction"`
	}
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, false, fmt.Errorf("%w: permit_join answer: %v", ErrInvalid, err)
	}
	txn, _ := r.Transaction.(string)
	switch r.Status {
	case "ok":
		return zigbee.JoinAnswer{Now: now, Txn: txn, OK: true}, true, nil
	case "error":
		return zigbee.JoinAnswer{Now: now, Txn: txn, Reason: r.Error}, true, nil
	}
	return nil, false, fmt.Errorf("%w: permit_join answer with status %q", ErrInvalid, r.Status)
}

// parseHealth reads Zigbee2MQTT's answer to a health check:
// {"data":{"healthy":true},"status":"ok","transaction":"..."}. Whatever it says,
// Zigbee2MQTT runs; only the transaction matters.
func parseHealth(payload []byte, now time.Time) (zigbee.Input, bool, error) {
	var r struct {
		Transaction any `json:"transaction"`
	}
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, false, fmt.Errorf("%w: health_check answer: %v", ErrInvalid, err)
	}
	txn, _ := r.Transaction.(string)
	return zigbee.Health{Now: now, Txn: txn}, true, nil
}

// ResponseError reads Zigbee2MQTT's answer to one of our requests: when it reports
// an error, it returns the request and the error, for the logs.
func ResponseError(base, topic string, payload []byte) (request, reason string, failed bool) {
	request, found := strings.CutPrefix(topic, base+"/bridge/response/")
	if !found {
		return "", "", false
	}
	var r struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if json.Unmarshal(payload, &r) != nil || r.Status != "error" {
		return "", "", false
	}
	return request, r.Error, true
}

// Build turns an action of the core into a message for Zigbee2MQTT. ok is false for
// an action that is not for Zigbee2MQTT.
func Build(base string, a zigbee.Action) (Message, bool, error) {
	var topic string
	var body any
	switch v := a.(type) {
	case zigbee.Send:
		topic, body = base+"/"+v.IEEE+"/set", v.Payload
	case zigbee.PermitJoin:
		// "value" for 1.x, "time" for both; zero closes the network. The
		// transaction comes back in the answer.
		req := map[string]any{"value": v.Seconds > 0, "time": v.Seconds}
		if v.Txn != "" {
			req["transaction"] = v.Txn
		}
		topic, body = base+"/bridge/request/permit_join", req
	case zigbee.HealthCheck:
		topic, body = base+"/bridge/request/health_check", map[string]any{"transaction": v.Txn}
	case zigbee.CarriesCode:
		// A device option, kept by Zigbee2MQTT in its configuration and read at each
		// message: its messages go out at QoS 0, over device_options' QoS 1.
		topic, body = base+"/bridge/request/device/options", map[string]any{"id": v.IEEE, "options": map[string]any{"qos": 0}}
	case zigbee.Rename:
		topic, body = base+"/bridge/request/device/rename", map[string]any{"from": v.IEEE, "to": v.To}
	case zigbee.Expel:
		req := map[string]any{"id": v.IEEE}
		if v.Force {
			req["force"] = true // out of Zigbee2MQTT's list, whether it answers or not
		}
		topic, body = base+"/bridge/request/device/remove", req
	default:
		return Message{}, false, nil
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Message{}, false, fmt.Errorf("z2m: encoding %T: %w", a, err)
	}
	return Message{Topic: topic, Payload: payload}, true, nil
}
