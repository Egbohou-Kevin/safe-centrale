package wire

import (
	"slices"
	"time"

	"safehome/internal/contract"
	"safehome/internal/health"
)

// deviceTypes is the order in which a device's capabilities decide what it is for the
// owner: a keypad also has a panic key, a smoke detector a tamper switch. Tamper,
// battery and link never decide. A siren's capability comes from its commands.
var deviceTypes = []string{
	contract.CapSiren, contract.CapKeypad, contract.CapSmoke, contract.CapContact,
	contract.CapMotion, contract.CapGlassBreak, contract.CapVibration, contract.CapPanic,
}

// deviceType returns the first capability of deviceTypes the device has.
func deviceType(capabilities []string) (string, bool) {
	for _, t := range deviceTypes {
		if slices.Contains(capabilities, t) {
			return t, true
		}
	}
	return "", false
}

// Registry turns the adapter's registry into the monitor's list of devices.
func Registry(d contract.RegistrySnapshotData) health.Registry {
	r := health.Registry{Devices: make(map[string][]string, len(d.Devices))}
	for _, dev := range d.Devices {
		if dev.DeviceID != "" {
			r.Devices[dev.DeviceID] = slices.Clone(dev.Capabilities)
		}
	}
	return r
}

// Devices builds system.devices from the monitor's list. A device none of whose
// capabilities decides a type (no profile today) is left out: it is neither a
// sensor, a keypad nor a siren. Since and heard go only with a device offline, both to
// the minute, which is all the app shows. The minute hides nothing: the envelope's ts
// of the list sent when a device goes offline gives its last news to the second
// (docs/contract.md, system.devices).
func Devices(devices []health.Device) contract.DevicesData {
	out := contract.DevicesData{Devices: []contract.DeviceHealth{}}
	for _, d := range devices {
		typ, ok := deviceType(d.Capabilities)
		if !ok {
			continue
		}
		dh := contract.DeviceHealth{
			ID: d.ID, Name: contract.DeviceName(d.Zone.Name), Zone: d.Zone.Kind, Type: typ,
			Online: d.Online, BatteryLow: d.BatteryLow,
		}
		if !d.Online {
			dh.Since, dh.Heard = contract.TSPtr(d.Since.Truncate(time.Minute)), contract.TSPtr(d.Heard.Truncate(time.Minute))
		}
		if d.BatteryKnown {
			dh.Battery = &d.Battery
		}
		out.Devices = append(out.Devices, dh)
	}
	return out
}
