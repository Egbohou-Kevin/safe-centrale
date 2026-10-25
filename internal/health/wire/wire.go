// Package wire translates between the health monitor's domain types and the event
// contract. The monitor never imports the contract, so a new contract version only
// changes this package.
package wire

import (
	"time"

	"safehome/internal/contract"
	"safehome/internal/health"
)

// Input turns a device event or state into a health input. ok is false when it does
// not concern supervision (a door opening, for instance) or is malformed.
func Input(d contract.DeviceEventData, now time.Time) (in health.Input, ok bool) {
	switch d.Capability {
	case contract.CapMains:
		return health.Mains{Now: now, Lost: d.StringValue() == contract.ValLost}, true
	case contract.CapBattery:
		percent, valid := d.IntValue()
		if !valid {
			return nil, false
		}
		return health.Battery{Now: now, DeviceID: d.DeviceID, Percent: percent}, true
	case contract.CapAvailability:
		return health.Availability{
			Now: now, DeviceID: d.DeviceID, Online: d.StringValue() == contract.ValOnline,
			Since: timeOf(d.Since), Heard: timeOf(d.Heard),
		}, true
	}
	return nil, false
}

// timeOf is the time of an optional field, zero when absent.
func timeOf(t *contract.Timestamp) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}

// GSM turns the GSM gateway's status, written at seen (the envelope's ts), into a
// health input: the backup is usable when at least one line answers, is on the
// network and has not run out of credit. The caller sets Now.
func GSM(d contract.GSMStatusData, seen time.Time) health.GSM {
	for _, l := range d.Lines {
		if l.Responding && l.Registered && l.Credit != contract.CreditEmpty {
			return health.GSM{Seen: seen, Usable: true}
		}
	}
	return health.GSM{Seen: seen}
}

// Zigbee turns the Zigbee adapter's status, written at seen (the envelope's ts), into
// a health input: the sensors can be heard while Zigbee2MQTT is online. The caller
// sets Now.
func Zigbee(d contract.ZigbeeStatusData, seen time.Time) health.Zigbee {
	return health.Zigbee{Seen: seen, Usable: d.Bridge == contract.BridgeOnline}
}

// Internet turns the link agent's status, written at seen (the envelope's ts), into a
// health input: the internet works while the agent is connected to the cloud. The
// caller sets Now.
func Internet(d contract.AgentStatusData, seen time.Time) health.Internet {
	return health.Internet{Seen: seen, Usable: d.Uplink == contract.UplinkUp}
}
