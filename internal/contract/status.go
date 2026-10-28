package contract

// HeartbeatData is the payload of system.heartbeat.
type HeartbeatData struct {
	BootID     string            `json:"boot_id"`
	UptimeS    int64             `json:"uptime_s"`
	Engine     string            `json:"engine"` // the alarm state
	QueueDepth int               `json:"queue_depth"`
	FW         map[string]string `json:"fw,omitempty"` // component -> version
	// ClockOK says whether the hub's clock is synchronised (NTP): after a power cut
	// without internet, the times it sends may be wrong. Absent when unknown.
	ClockOK *bool `json:"clock_ok,omitempty"`
}

// GSMLine is the state of one GSM line (a modem and its SIM).
type GSMLine struct {
	ID         string     `json:"id"`
	Operator   string     `json:"operator,omitempty"`
	Responding bool       `json:"responding"` // the modem answers
	Registered bool       `json:"registered"`
	Signal     int        `json:"signal"`           // 0 to 100
	Credit     string     `json:"credit,omitempty"` // CreditOK, CreditEmpty, or absent when unknown
	LastTest   *Timestamp `json:"last_test,omitempty"`
	LastTestOK *bool      `json:"last_test_ok,omitempty"`
	Restarts   int        `json:"restarts,omitempty"` // modem restarts, counted across the gateway's restarts
}

// The credit of a GSM line.
const (
	CreditOK    = "ok"
	CreditEmpty = "empty" // an operator said so
)

// GSMStatusData is the payload of gsm.status: the GSM gateway's lines, published
// retained at every change. The hub copies the lines into system.health.
type GSMStatusData struct {
	Lines  []GSMLine `json:"lines"`
	Queued int       `json:"queued"` // SMS and calls waiting
}

// AgentStatusData is the payload of agent.status: the link to the cloud, published
// retained at every change and repeated every minute. The hub turns it into the
// internet link of system.health; the escalation uses it to go straight to the GSM
// channels when the cloud cannot be reached.
type AgentStatusData struct {
	Uplink    string `json:"uplink"`              // "up" or "down"
	Transport string `json:"transport,omitempty"` // the URL scheme in use: "mqtts", "wss"...
	Queued    int    `json:"queued"`              // messages waiting for the cloud
	OldestS   int    `json:"oldest_s"`            // age of the oldest one, in seconds
	Dropped   int    `json:"dropped"`             // messages dropped (queue full, too old, refused), counted across restarts
	// Economy: on mobile data paid by the client, the heartbeat and this status reach
	// the cloud only every 5 minutes (a change of the link goes at once).
	Economy bool `json:"economy,omitempty"`
	// Key is the link agent's public key (P-256, 65 bytes in base64): the phones seal
	// the access code with it (internal/sealcode), so that the cloud never sees it.
	Key string `json:"key,omitempty"`
}

// GSMHealth groups the GSM lines.
type GSMHealth struct {
	Lines []GSMLine `json:"lines"`
}

// HealthData is the payload of system.health, published retained at every change.
type HealthData struct {
	Mains      string            `json:"mains,omitempty"`       // "present" or "lost"
	HubBattery *int              `json:"hub_battery,omitempty"` // percent, absent if unknown
	Links      map[string]string `json:"links"`                 // zigbee, gsm, internet -> ok, down...
	GSM        *GSMHealth        `json:"gsm,omitempty"`
}

// DevicesData is the payload of system.devices: the health of each registered device,
// for the owner's list of sensors. Published retained at every real change, never for
// a door or a passer-by as such: no door or motion state, and no time for a device
// that answers. Its envelope's ts still dates each change it went for, such as a
// device going offline or its first message back (see docs/contract.md).
type DevicesData struct {
	Devices []DeviceHealth `json:"devices"`
	// Sirens is how many sirens the site's configuration names (siren_ids), paired or
	// not (since 26 October 2026). 0: the site has none, by choice: its alarms sound
	// nothing, the hub tells no siren cycle and no siren_failed, and the cloud and the
	// app never say a siren sounds. Absent from an older hub: unknown.
	Sirens *int `json:"sirens,omitempty"`
}

// DeviceHealth is one device of system.devices.
type DeviceHealth struct {
	ID string `json:"id"` // the registry's device_id
	// Name and Zone come from the device's zone in the site's configuration: its name
	// (see DeviceName) and its kind (entry, instant...). Absent for a device outside
	// any zone (a siren).
	Name string `json:"name,omitempty"`
	Zone string `json:"zone,omitempty"`
	// Room is the room the installer put the device in, set with its zone (since 28
	// October 2026), cut like Name. Absent outside any zone and for a zone without one.
	Room string `json:"room,omitempty"`
	Type string `json:"type"` // contact, motion, smoke, keypad, siren, panic, vibration, glass_break
	// Online is false once the adapter counts the device offline. Only then: Since,
	// when it went offline, and Heard, its last news before (absent when it said
	// nothing since the adapter's restart), both to the minute and fixed while it is
	// offline.
	Online bool       `json:"online"`
	Since  *Timestamp `json:"since,omitempty"`
	Heard  *Timestamp `json:"heard,omitempty"`
	// Battery is the last reading in percent, rounded to the nearest ten, absent when
	// none was read; BatteryLow is present, and true, while the battery is weak.
	Battery    *int `json:"battery,omitempty"`
	BatteryLow bool `json:"battery_low,omitempty"`
}

// ContactsData is the payload of system.contacts: the site's contacts, in the order
// the escalation plan of an intrusion tells them (the plan the hub really follows),
// for the people the app shows. Never a phone number.
type ContactsData struct {
	Contacts []ContactTold `json:"contacts"`
}

// ContactTold is one contact of system.contacts. ToldAfterS is how long after the
// alarm an intrusion first tells them, in seconds; nil when it never does.
type ContactTold struct {
	ID         string   `json:"id"`
	Name       string   `json:"name,omitempty"`
	Role       string   `json:"role"`
	Channels   []string `json:"channels"`
	ToldAfterS *int     `json:"told_after_s,omitempty"`
}
