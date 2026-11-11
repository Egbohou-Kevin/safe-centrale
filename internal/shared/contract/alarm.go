package contract

import (
	"fmt"
	"log/slog"
	"slices"
	"unicode/utf8"
)

// CommandArgs are the arguments of an order to the engine. The access code is
// never printed or written to a log: String, GoString and LogValue mask it.
type CommandArgs struct {
	Code string `json:"code,omitempty"`
	// Sealed is the code sealed by the phone for the link agent (internal/sealcode): all
	// the cloud carries. The agent opens it and gives the engine Code, never Sealed.
	Sealed  string   `json:"sealed,omitempty"`
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

// NeedsCode reports whether the command requires a valid code.
func NeedsCode(name string) bool { return name == CmdDisarm || name == CmdSilence || name == CmdEnd }

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
	// Incidents are the incidents this disarm ended (the alarm ringing, and the
	// intrusions and tampers of the arming whose siren already ran out); empty when
	// it ended none. Their messages have no reason to go any more.
	Incidents []string `json:"incidents"`
}

// EntryDelayData: alarm.entry_delay.
type EntryDelayData struct {
	DeviceID   string    `json:"device_id"`
	DeviceName string    `json:"device_name,omitempty"` // see DeviceName
	EndsAt     Timestamp `json:"ends_at"`
}

// TriggeredData: alarm.triggered. Incident ties the event to its alerts.
type TriggeredData struct {
	Incident   string `json:"incident"`
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name,omitempty"` // see DeviceName
	Capability string `json:"capability"`
	Zone       string `json:"zone"`
}

// MaxDeviceName bounds device_name, in characters: as long as the installer lets a
// zone name be (it goes into SMS too).
const MaxDeviceName = 32

// DeviceName returns a device's name as the alarm events carry it (device_name):
// the name the site configuration gives it ("Front door"), cut to MaxDeviceName
// characters. Empty, the field is absent: a device without a zone has no name.
func DeviceName(name string) string {
	if utf8.RuneCountInString(name) <= MaxDeviceName {
		return name
	}
	return string([]rune(name)[:MaxDeviceName])
}

// SirenCycleData: alarm.siren_cycle.
type SirenCycleData struct {
	Incident string `json:"incident"`
	Cycle    int    `json:"cycle"`
	Of       int    `json:"of"`
}

// SirenFailedData: alarm.siren_failed, a siren refused to sound during an alarm,
// resends included. Said once per siren and per alarm, for the history; the
// contacts' alert is an alert.request siren_failed under an incident of its own.
type SirenFailedData struct {
	Incident string `json:"incident"`  // the alarm the siren should have sounded for
	DeviceID string `json:"device_id"` // the silent siren
}

// ClearedData: alarm.cleared.
type ClearedData struct {
	Incident  string `json:"incident"`
	Origin    string `json:"origin"` // "user" or "timeout"
	User      string `json:"user,omitempty"`
	DurationS int    `json:"duration_s"`
	// Ended: the alarm was ended with alarm.end, the shop still armed, whether its
	// siren still rang or had run out; absent otherwise.
	Ended bool `json:"ended,omitempty"`
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

// AlarmStateData: alarm.state, published retained. While triggered, Incident names
// the incident that is ringing, Siren says whether its schedule is in a cycle
// (SirenOn) or in the pause between two (SirenPaused), and EndsAt when that cycle or
// pause ends. Siren is the engine's schedule, not the siren itself: a silent siren is
// told by alarm.siren_failed. ExitDelayS is the exit delay the next arming would
// take, in every state, absent when the hub does not know it (never 0).
type AlarmStateData struct {
	State         string     `json:"state"`
	Since         Timestamp  `json:"since"`
	EndsAt        *Timestamp `json:"ends_at,omitempty"`
	Incident      string     `json:"incident,omitempty"`
	Siren         string     `json:"siren,omitempty"`
	Cycle         int        `json:"cycle,omitempty"`
	Of            int        `json:"of,omitempty"`
	LockedUntil   *Timestamp `json:"locked_until,omitempty"`
	NextScheduled *Timestamp `json:"next_scheduled,omitempty"`
	ExitDelayS    int        `json:"exit_delay_s,omitempty"`
}
