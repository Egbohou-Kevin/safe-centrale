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

// Filter is the subscription that receives everything Zigbee2MQTT publishes.
func Filter(base string) string { return base + "/#" }

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
			return parseState(payload, now)
		case "event":
			return parseEvent(payload, now)
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
	return zigbee.Report{Now: now, Name: rest, Fields: fields, Retained: retained}, true, nil
}

// parseState reads bridge/state: "online" or {"state": "online"} depending on the
// version.
func parseState(payload []byte, now time.Time) (zigbee.Input, bool, error) {
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
		return zigbee.Bridge{Now: now, Online: true}, true, nil
	case "offline":
		return zigbee.Bridge{Now: now}, true, nil
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
	if e.Data.IEEE == "" && (e.Type == "device_interview" || e.Type == "device_leave") {
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
	}
	return nil, false, nil // device_joined, device_announce, interview started...
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
		// "value" for 1.x, "time" for both; zero closes the network.
		topic, body = base+"/bridge/request/permit_join", map[string]any{"value": v.Seconds > 0, "time": v.Seconds}
	case zigbee.Rename:
		topic, body = base+"/bridge/request/device/rename", map[string]any{"from": v.IEEE, "to": v.To}
	case zigbee.Expel:
		topic, body = base+"/bridge/request/device/remove", map[string]any{"id": v.IEEE}
	default:
		return Message{}, false, nil
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Message{}, false, fmt.Errorf("z2m: encoding %T: %w", a, err)
	}
	return Message{Topic: topic, Payload: payload}, true, nil
}
