package contract

import "fmt"

// RegistryPairData: registry.pair, open the network to new devices for DurationS
// seconds. Zero closes it.
type RegistryPairData struct {
	DurationS int `json:"duration_s"`
}

// Validate checks the order.
func (d RegistryPairData) Validate() error {
	if d.DurationS < 0 {
		return fmt.Errorf("%w: negative duration_s", ErrInvalid)
	}
	return nil
}

// RegistryRemoveData: registry.remove.
type RegistryRemoveData struct {
	DeviceID string `json:"device_id"`
}

// Validate checks the order.
func (d RegistryRemoveData) Validate() error {
	if d.DeviceID == "" {
		return fmt.Errorf("%w: empty device_id", ErrInvalid)
	}
	return nil
}

// RegistryDevice says what a device is. What it is for (name, zone) belongs to the
// site's configuration, read by the engine.
type RegistryDevice struct {
	DeviceID     string   `json:"device_id"`
	Model        string   `json:"model"` // the profile, for example aqara.contact.mccgq11lm
	Capabilities []string `json:"capabilities"`
	Adapter      string   `json:"adapter"`
}

// RegistrySnapshotData: registry.snapshot, retained on registry/devices.
type RegistrySnapshotData struct {
	Devices []RegistryDevice `json:"devices"`
}

// RegistryDeviceAddedData: registry.device_added. Ref is the pairing order.
type RegistryDeviceAddedData struct {
	Ref          string   `json:"ref,omitempty"`
	DeviceID     string   `json:"device_id"`
	Model        string   `json:"model"`
	Capabilities []string `json:"capabilities"`
}

// RegistryDeviceRejectedData: registry.device_rejected. Model is the hardware's
// model, empty when it could not be read.
type RegistryDeviceRejectedData struct {
	Ref    string `json:"ref,omitempty"`
	Model  string `json:"model"`
	Reason string `json:"reason"`
}

// RegistryDeviceRemovedData: registry.device_removed. Ref is the removal order.
type RegistryDeviceRemovedData struct {
	Ref      string `json:"ref,omitempty"`
	DeviceID string `json:"device_id"`
}

// ZigbeeStatusData is the payload of zigbee.status: the Zigbee network, published
// retained at every change and repeated every minute. The hub turns it into the
// zigbee link of system.health.
type ZigbeeStatusData struct {
	Bridge  string   `json:"bridge"`  // online, offline
	Devices int      `json:"devices"` // registered devices
	Offline []string `json:"offline"` // devices that stopped answering, sorted
	Pairing bool     `json:"pairing"` // the network is open to new devices
}
