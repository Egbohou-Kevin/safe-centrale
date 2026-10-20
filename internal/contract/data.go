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

// Validate checks the capability and its value.
func (d DeviceEventData) Validate() error {
	if d.DeviceID == "" {
		return fmt.Errorf("%w: empty device_id", ErrInvalid)
	}
	if d.Code != "" {
		if d.Capability != CapKeypad || (d.StringValue() != ValDisarm && d.StringValue() != ValSilence) {
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

// CommandArgs are the arguments of an order to the engine. The access code is
// never printed or written to a log: String, GoString and LogValue mask it.
type CommandArgs struct {
	Code    string   `json:"code,omitempty"`
	Force   bool     `json:"force,omitempty"`
	Devices []string `json:"devices,omitempty"`
}

func (a CommandArgs) redacted() CommandArgs {
	if a.Code != "" {
		a.Code = "***"
	}
	return a
}

// String masks the code.
func (a CommandArgs) String() string {
	r := a.redacted()
	return fmt.Sprintf("{code:%q force:%t devices:%v}", r.Code, r.Force, r.Devices)
}

// GoString masks the code for %#v.
func (a CommandArgs) GoString() string { return a.String() }

// LogValue masks the code in slog logs.
func (a CommandArgs) LogValue() slog.Value {
	r := a.redacted()
	return slog.GroupValue(
		slog.String("code", r.Code),
		slog.Bool("force", r.Force),
		slog.Any("devices", r.Devices),
	)
}

// AlarmCommandData is the payload of alarm.command.
type AlarmCommandData struct {
	Command string      `json:"command"`
	Args    CommandArgs `json:"args"`
	Origin  string      `json:"origin"`
	User    string      `json:"user,omitempty"`
	Exp     *Timestamp  `json:"exp,omitempty"`
}

var alarmCommands = []string{CmdArm, CmdArmNow, CmdDisarm, CmdSilence, CmdBypass, CmdPostpone}
var origins = []string{OriginKeypad, OriginApp, OriginSchedule}

// Validate checks the shape of the order. An unknown command is not a shape error:
// the engine answers it with a failure acknowledgement.
func (d AlarmCommandData) Validate() error {
	if d.Command == "" {
		return fmt.Errorf("%w: empty command", ErrInvalid)
	}
	if !slices.Contains(origins, d.Origin) {
		return fmt.Errorf("%w: origin %q", ErrInvalid, d.Origin)
	}
	return nil
}

// KnownAlarmCommand reports whether the engine knows this command.
func KnownAlarmCommand(name string) bool { return slices.Contains(alarmCommands, name) }

// NeedsCode reports whether the command requires a valid code.
func NeedsCode(name string) bool { return name == CmdDisarm || name == CmdSilence }

// AlarmAckData is the payload of alarm.ack. It never contains the code.
type AlarmAckData struct {
	Ref    string         `json:"ref"`
	Status string         `json:"status"`
	Reason string         `json:"reason,omitempty"`
	Detail map[string]any `json:"detail,omitempty"`
}

// ArmingData: alarm.arming, start of the exit delay.
type ArmingData struct {
	Origin   string    `json:"origin"`
	User     string    `json:"user,omitempty"`
	EndsAt   Timestamp `json:"ends_at"`
	Forced   bool      `json:"forced"`
	Bypassed []string  `json:"bypassed"`
	// Faults known when the arming started: arming goes on, but someone should look.
	Offline    []string `json:"offline,omitempty"`     // devices that stopped answering
	LowBattery []string `json:"low_battery,omitempty"` // devices with a weak battery
}

// ArmedData: alarm.armed.
type ArmedData struct {
	Origin string `json:"origin"`
	User   string `json:"user,omitempty"`
	Mode   string `json:"mode,omitempty"` // absent in v1
}

// DisarmedData: alarm.disarmed.
type DisarmedData struct {
	Origin string `json:"origin"`
	User   string `json:"user,omitempty"`
}

// EntryDelayData: alarm.entry_delay.
type EntryDelayData struct {
	DeviceID string    `json:"device_id"`
	EndsAt   Timestamp `json:"ends_at"`
}

// TriggeredData: alarm.triggered. Incident ties the event to its alerts.
type TriggeredData struct {
	Incident   string `json:"incident"`
	DeviceID   string `json:"device_id"`
	Capability string `json:"capability"`
	Zone       string `json:"zone"`
}

// SirenCycleData: alarm.siren_cycle.
type SirenCycleData struct {
	Incident string `json:"incident"`
	Cycle    int    `json:"cycle"`
	Of       int    `json:"of"`
}

// ClearedData: alarm.cleared.
type ClearedData struct {
	Incident  string `json:"incident"`
	Origin    string `json:"origin"` // "user" or "timeout"
	User      string `json:"user,omitempty"`
	DurationS int    `json:"duration_s"`
}

// AuthFailedData: alarm.auth_failed.
type AuthFailedData struct {
	Origin      string     `json:"origin"`
	Attempts    int        `json:"attempts"`
	LockedUntil *Timestamp `json:"locked_until,omitempty"`
}

// ArmingScheduledData: alarm.arming_scheduled, advance notice of the automatic arming.
type ArmingScheduledData struct {
	ScheduledFor Timestamp `json:"scheduled_for"`
}

// PostponedData: alarm.postponed, the automatic arming was pushed back.
type PostponedData struct {
	ScheduledFor Timestamp `json:"scheduled_for"`
	Count        int       `json:"count"`
	Max          int       `json:"max"`
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
	Signal     int        `json:"signal"` // 0 to 100
	Credit     string     `json:"credit,omitempty"`
	LastTest   *Timestamp `json:"last_test,omitempty"`
	LastTestOK *bool      `json:"last_test_ok,omitempty"`
	Restarts   int        `json:"restarts,omitempty"` // modem restarts since the gateway started
}

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
	Dropped   int    `json:"dropped"`             // messages dropped since the start: the queue was full
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

// AlarmStateData: alarm.state, published retained.
type AlarmStateData struct {
	State         string     `json:"state"`
	Since         Timestamp  `json:"since"`
	EndsAt        *Timestamp `json:"ends_at,omitempty"`
	Cycle         int        `json:"cycle,omitempty"`
	Of            int        `json:"of,omitempty"`
	LockedUntil   *Timestamp `json:"locked_until,omitempty"`
	NextScheduled *Timestamp `json:"next_scheduled,omitempty"`
}

// AlertRequestData: alert.request.
type AlertRequestData struct {
	Incident string   `json:"incident"`
	Kind     string   `json:"kind"`
	Urgency  string   `json:"urgency"`
	Percent  int      `json:"percent,omitempty"` // battery level, for battery alerts
	Devices  []string `json:"devices,omitempty"`
	Wave     int      `json:"wave,omitempty"`
	Contact  string   `json:"contact,omitempty"`
	Channel  string   `json:"channel,omitempty"`
	Text     string   `json:"text,omitempty"`
}

// AlertResultData: alert.result.
type AlertResultData struct {
	Ref    string `json:"ref"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// AlertAckData: alert.ack.
type AlertAckData struct {
	Incident string `json:"incident"`
	Contact  string `json:"contact"`
	Via      string `json:"via"` // "sms" or "app"
}
