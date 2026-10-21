package contract

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
)

// DeviceEventData is the payload of device.event and device.state.
//
// Code is the access code typed on a keypad. It is present only for a keypad
// "disarm" or "silence", and like every access code it is never printed or logged:
// String, GoString and LogValue mask it.
type DeviceEventData struct {
	DeviceID   string          `json:"device_id"`
	Capability string          `json:"capability"`
	Value      json.RawMessage `json:"value"`
	Since      *Timestamp      `json:"since,omitempty"` // device.state only
	Code       string          `json:"code,omitempty"`  // keypad disarm and silence only
}

func (d DeviceEventData) redacted() DeviceEventData {
	if d.Code != "" {
		d.Code = "***"
	}
	return d
}

// String masks the code.
func (d DeviceEventData) String() string {
	r := d.redacted()
	return fmt.Sprintf("{device:%q capability:%q value:%s code:%q}", r.DeviceID, r.Capability, r.Value, r.Code)
}

// GoString masks the code for %#v.
func (d DeviceEventData) GoString() string { return d.String() }

// LogValue masks the code in slog logs.
func (d DeviceEventData) LogValue() slog.Value {
	r := d.redacted()
	return slog.GroupValue(
		slog.String("device_id", r.DeviceID),
		slog.String("capability", r.Capability),
		slog.String("value", string(r.Value)),
		slog.String("code", r.Code),
	)
}

// maxCodeLen bounds what a keypad may send; the access registry decides what a
// valid code is.
const maxCodeLen = 16

// StringValue returns the value of an enumerated capability.
func (d DeviceEventData) StringValue() string {
	var s string
	if err := json.Unmarshal(d.Value, &s); err != nil {
		return ""
	}
	return s
}

// IntValue returns the value of a numeric capability (battery, link).
func (d DeviceEventData) IntValue() (int, bool) {
	n, err := strconv.Atoi(string(d.Value))
	return n, err == nil
}

// CodeAllowed reports whether a code may go with this value of a capability: only
// with a keypad "disarm" or "silence", the keys whose order needs one.
func CodeAllowed(capability, value string) bool {
	return capability == CapKeypad && (value == ValDisarm || value == ValSilence)
}

// Validate checks the capability and its value.
func (d DeviceEventData) Validate() error {
	if d.DeviceID == "" {
		return fmt.Errorf("%w: empty device_id", ErrInvalid)
	}
	if d.Code != "" {
		if !CodeAllowed(d.Capability, d.StringValue()) {
			return fmt.Errorf("%w: a code is only allowed with keypad disarm or silence", ErrInvalid)
		}
		if len(d.Code) > maxCodeLen || strings.Trim(d.Code, "0123456789") != "" {
			return fmt.Errorf("%w: the code must be digits, at most %d", ErrInvalid, maxCodeLen)
		}
	}
	if numericCapabilities[d.Capability] {
		n, ok := d.IntValue()
		if !ok || n < 0 || n > 100 {
			return fmt.Errorf("%w: %s must be an integer from 0 to 100, got %s", ErrInvalid, d.Capability, d.Value)
		}
		return nil
	}
	allowed, known := allowedValues[d.Capability]
	if !known {
		return fmt.Errorf("%w: unknown capability %q", ErrInvalid, d.Capability)
	}
	if !slices.Contains(allowed, d.StringValue()) {
		return fmt.Errorf("%w: value %s not allowed for %s", ErrInvalid, d.Value, d.Capability)
	}
	return nil
}

// DeviceCommandData is the payload of a command to a device (type "command", topic
// device/{id}/cmd). Only the alarm engine sends them.
type DeviceCommandData struct {
	DeviceID string            `json:"device_id"`
	Command  string            `json:"command"`
	Args     DeviceCommandArgs `json:"args"`
	Exp      *Timestamp        `json:"exp,omitempty"`
}

// DeviceCommandArgs are the arguments of a device command.
type DeviceCommandArgs struct {
	DurationS int `json:"duration_s,omitempty"` // siren.on: how long it sounds
}

var deviceCommands = []string{CmdSirenOn, CmdSirenOff, CmdDeviceIdentify}

// Validate checks the shape of the command.
func (d DeviceCommandData) Validate() error {
	if d.DeviceID == "" {
		return fmt.Errorf("%w: empty device_id", ErrInvalid)
	}
	if !slices.Contains(deviceCommands, d.Command) {
		return fmt.Errorf("%w: unknown device command %q", ErrInvalid, d.Command)
	}
	if d.Command == CmdSirenOn && d.Args.DurationS < 1 {
		return fmt.Errorf("%w: siren.on needs a duration", ErrInvalid)
	}
	return nil
}

// Statuses of a device command acknowledgement.
const (
	CmdAccepted = "accepted"
	CmdDone     = "done"
	CmdFailed   = "failed"
	CmdExpired  = "expired"
)

// CommandAckData is the payload of command.ack (topic device/{id}/ack).
type CommandAckData struct {
	Ref    string `json:"ref"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}
