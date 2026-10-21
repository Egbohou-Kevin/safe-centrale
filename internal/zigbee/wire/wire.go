// Package wire translates between the zigbee core and the event contract: the core
// speaks of devices and capabilities as plain strings, this package makes contract
// payloads of them, checked against the contract's rules.
package wire

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"safehome/internal/contract"
	"safehome/internal/zigbee"
)

// Device is the device.event and the device.state a change makes; either may be nil.
type Device struct {
	Event *contract.DeviceEventData
	State *contract.DeviceEventData
}

// Changed builds the messages of a capability change. A value the contract does not
// allow (a mistake in a profile, a keypad code that is not digits) is an error: the
// message is not sent. The error never contains the code. A code goes only with the
// keys that take one (contract.CodeAllowed): any other key keeps its action and loses
// the code, since a keypad may send the code typed before "arm" (the Develco
// KEPZB-110 does).
func Changed(c zigbee.Changed) (Device, error) {
	value, err := encode(c.Value, c.Numeric)
	if err != nil {
		return Device{}, err
	}
	ev := contract.DeviceEventData{DeviceID: c.DeviceID, Capability: c.Capability, Value: value}
	if contract.CodeAllowed(c.Capability, c.Value) {
		ev.Code = c.Code
	}
	if err := ev.Validate(); err != nil {
		return Device{}, fmt.Errorf("wire: %s of %s: %w", c.Capability, c.DeviceID, err)
	}
	var out Device
	if c.Notify {
		out.Event = &ev
	}
	if c.State {
		st := ev
		st.Code = "" // a code is never kept
		st.Since = contract.TSPtr(c.Since)
		out.State = &st
	}
	return out, nil
}

// Presence builds the availability event and state of a device.
func Presence(p zigbee.Presence) Device {
	v := contract.ValOffline
	if p.Online {
		v = contract.ValOnline
	}
	ev := contract.DeviceEventData{DeviceID: p.DeviceID, Capability: contract.CapAvailability, Value: json.RawMessage(strconv.Quote(v))}
	st := ev
	st.Since = contract.TSPtr(p.Since)
	return Device{Event: &ev, State: &st}
}

func encode(v string, numeric bool) (json.RawMessage, error) {
	if !numeric {
		return json.RawMessage(strconv.Quote(v)), nil
	}
	if _, err := strconv.Atoi(v); err != nil {
		return nil, fmt.Errorf("wire: %q is not a number", v)
	}
	return json.RawMessage(v), nil
}

// StateCapabilities lists the retained states of a device, to clear them when it is
// removed: those of its profile, and its availability.
func StateCapabilities(d zigbee.Device) []string {
	out := slices.Clone(d.Capabilities)
	if !slices.Contains(out, contract.CapAvailability) {
		out = append(out, contract.CapAvailability)
	}
	slices.Sort(out)
	return out
}

var ackStatuses = map[string]string{
	zigbee.AckAccepted: contract.CmdAccepted,
	zigbee.AckDone:     contract.CmdDone,
	zigbee.AckFailed:   contract.CmdFailed,
	zigbee.AckExpired:  contract.CmdExpired,
}

// Ack builds a command.ack.
func Ack(a zigbee.Ack) (contract.CommandAckData, error) {
	s, ok := ackStatuses[a.Status]
	if !ok {
		return contract.CommandAckData{}, fmt.Errorf("wire: unknown command status %q", a.Status)
	}
	return contract.CommandAckData{Ref: a.Ref, Status: s, Reason: a.Reason}, nil
}

// Command turns a device command into an input. ref is the envelope id.
func Command(d contract.DeviceCommandData, ref string, now time.Time) zigbee.Command {
	c := zigbee.Command{Now: now, Ref: ref, DeviceID: d.DeviceID, Name: d.Command, DurationS: d.Args.DurationS}
	if d.Exp != nil {
		c.Exp = d.Exp.Time
	}
	return c
}

// Pair turns registry.pair into an input.
func Pair(d contract.RegistryPairData, ref string, now time.Time) zigbee.Pair {
	return zigbee.Pair{Now: now, Ref: ref, For: time.Duration(d.DurationS) * time.Second}
}

// Remove turns registry.remove into an input.
func Remove(d contract.RegistryRemoveData, ref string, now time.Time) zigbee.Remove {
	return zigbee.Remove{Now: now, Ref: ref, DeviceID: d.DeviceID}
}

// RegistryDevice describes a device for the registry: its profile is its model. The
// IEEE address stays in the adapter.
func RegistryDevice(d zigbee.Device) contract.RegistryDevice {
	return contract.RegistryDevice{
		DeviceID: d.ID, Model: d.Profile, Capabilities: slices.Clone(d.Capabilities), Adapter: contract.AdapterZigbee,
	}
}

// Snapshot builds registry.snapshot.
func Snapshot(devices []zigbee.Device) contract.RegistrySnapshotData {
	out := contract.RegistrySnapshotData{Devices: []contract.RegistryDevice{}}
	for _, d := range devices {
		out.Devices = append(out.Devices, RegistryDevice(d))
	}
	return out
}

// Added builds registry.device_added.
func Added(a zigbee.Added) contract.RegistryDeviceAddedData {
	return contract.RegistryDeviceAddedData{
		Ref: a.Ref, DeviceID: a.Device.ID, Model: a.Device.Profile, Capabilities: slices.Clone(a.Device.Capabilities),
	}
}

var rejectReasons = map[string]string{
	zigbee.RejectUnknownModel:    contract.RejectUnknownModel,
	zigbee.RejectNotPairing:      contract.RejectNotPairing,
	zigbee.RejectInterviewFailed: contract.RejectInterviewFailed,
}

// Rejected builds registry.device_rejected.
func Rejected(r zigbee.Rejected) (contract.RegistryDeviceRejectedData, error) {
	reason, ok := rejectReasons[r.Reason]
	if !ok {
		return contract.RegistryDeviceRejectedData{}, fmt.Errorf("wire: unknown rejection %q", r.Reason)
	}
	return contract.RegistryDeviceRejectedData{Ref: r.Ref, Model: r.Model, Reason: reason}, nil
}

// Removed builds registry.device_removed.
func Removed(r zigbee.Removed) contract.RegistryDeviceRemovedData {
	return contract.RegistryDeviceRemovedData{Ref: r.Ref, DeviceID: r.Device.ID}
}

// Status builds zigbee.status.
func Status(s zigbee.Status) contract.ZigbeeStatusData {
	bridge := contract.BridgeOffline
	if s.Bridge {
		bridge = contract.BridgeOnline
	}
	offline := slices.Clone(s.Offline)
	if offline == nil {
		offline = []string{}
	}
	return contract.ZigbeeStatusData{Bridge: bridge, Devices: s.Devices, Offline: offline, Pairing: s.Pairing}
}
