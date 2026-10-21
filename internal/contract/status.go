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
