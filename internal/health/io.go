package health

import "time"

// Input is what the monitor receives: a reading from the hub or a sensor, or a
// clock tick. Every input carries the time it happened; the package never reads
// the clock.
type Input interface{ at() time.Time }

// Tick advances the clock. The driver sends one at the time returned by NextWake.
type Tick struct{ Now time.Time }

// Mains is a change of the mains supply, as seen by the hub battery module.
type Mains struct {
	Now  time.Time
	Lost bool
}

// Battery is a battery reading in percent: the hub's if DeviceID is the hub
// battery module, a sensor's otherwise.
type Battery struct {
	Now      time.Time
	DeviceID string
	Percent  int
}

// Availability says whether a device is answering. The adapter decides when a
// silent device counts as offline; this package only records it.
//
// For an offline device, the adapter also says since when (Since) and when it last
// heard it (Heard, zero when it said nothing since the adapter's restart): the device
// list shows them. Either may be unknown (zero).
type Availability struct {
	Now      time.Time
	DeviceID string
	Online   bool
	Since    time.Time
	Heard    time.Time
}

// Registry gives the devices the adapter registered, with their capabilities (for
// example "contact", "battery"): the device list shows those. It replaces the
// previous list.
type Registry struct {
	Now     time.Time
	Devices map[string][]string // device id -> capabilities
}

// Forget removes a device that left the registry, with its faults.
type Forget struct {
	Now      time.Time
	DeviceID string
}

// GSM says whether the GSM gateway can still send: at least one line answers, is on
// the network and has credit. Without it, the SMS and calls of an alarm cannot go.
//
// The gateway repeats its status even when nothing changed. Seen is when it wrote
// this one (zero means Now): a status older than the last one is ignored, and a
// gateway that stays silent for Config.GSMSilentAfter counts as down, because a dead
// or stuck gateway cannot say so itself.
type GSM struct {
	Now    time.Time
	Seen   time.Time
	Usable bool
}

func (g GSM) at() time.Time { return g.Now }

// Zigbee says whether the Zigbee adapter can hear the sensors: Zigbee2MQTT and its
// radio stick are up. Like the GSM gateway, the adapter repeats its state, and a
// silent adapter counts as down after Config.ZigbeeSilentAfter.
type Zigbee struct {
	Now    time.Time
	Seen   time.Time
	Usable bool
}

func (z Zigbee) at() time.Time { return z.Now }

// Internet says whether the link agent reaches the cloud. Like the others, the agent
// repeats its state, and a silent agent counts as down after
// Config.InternetSilentAfter.
type Internet struct {
	Now    time.Time
	Seen   time.Time
	Usable bool
}

func (i Internet) at() time.Time { return i.Now }

func (t Tick) at() time.Time         { return t.Now }
func (m Mains) at() time.Time        { return m.Now }
func (b Battery) at() time.Time      { return b.Now }
func (a Availability) at() time.Time { return a.Now }
func (r Registry) at() time.Time     { return r.Now }
func (f Forget) at() time.Time       { return f.Now }

// Action is what the monitor asks the driver to do.
type Action interface{ isAction() }

// AlertKind is the type of problem an alert reports.
type AlertKind string

const (
	AlertHubBatteryLow AlertKind = "hub_battery_low"
	AlertMainsRestored AlertKind = "mains_restored"
	AlertDeviceOffline AlertKind = "device_offline"
	AlertBatteryLow    AlertKind = "battery_low"
	// AlertGSMDegraded: the SMS backup cannot work (no credit, no network, dead
	// modem). The owner must act: recharge, check the SIM.
	AlertGSMDegraded AlertKind = "gsm_degraded"
	// AlertZigbeeDown: the sensors cannot be heard (Zigbee2MQTT, its radio stick or
	// the adapter stopped). The alarm is blind until it is fixed.
	AlertZigbeeDown AlertKind = "zigbee_down"
)

// Urgency ranks how fast someone must react to an alert.
type Urgency string

const (
	UrgencyCritical Urgency = "critical"
	UrgencyHigh     Urgency = "high"
	UrgencyLow      Urgency = "low"
)

// Alert asks for a message to the contacts.
type Alert struct {
	Incident string
	Kind     AlertKind
	Urgency  Urgency
	Percent  int      // battery level, 0 when it does not apply or is unknown
	Devices  []string // devices concerned, sorted
}

func (Alert) isAction() {}
