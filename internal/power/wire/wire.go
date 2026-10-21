// Package wire translates the battery module core's reports into the event contract:
// the mains and battery capabilities of the hub's own device.
package wire

import (
	"encoding/json"
	"strconv"

	"safehome/internal/contract"
	"safehome/internal/power"
)

// Mains turns a report of the supply into the state of deviceID's mains.
func Mains(deviceID string, m power.Mains) contract.DeviceEventData {
	v := contract.ValPresent
	if m.Lost {
		v = contract.ValLost
	}
	raw, _ := json.Marshal(v) // a string always encodes
	return contract.DeviceEventData{DeviceID: deviceID, Capability: contract.CapMains, Value: raw}
}

// Battery turns a report of the charge into the state of deviceID's battery.
func Battery(deviceID string, b power.Battery) contract.DeviceEventData {
	return contract.DeviceEventData{DeviceID: deviceID, Capability: contract.CapBattery, Value: json.RawMessage(strconv.Itoa(b.Percent))}
}
