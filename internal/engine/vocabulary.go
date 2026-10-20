package engine

// This file holds the domain vocabulary. It is deliberately independent of the
// wire format: the translation to and from the event contract lives in the wire
// subpackage, so a new contract version never forces a change here.

// Mode is the state of the alarm.
type Mode string

const (
	ModeDisarmed   Mode = "disarmed"
	ModeArming     Mode = "arming"
	ModeArmed      Mode = "armed"
	ModeEntryDelay Mode = "entry_delay"
	ModeTriggered  Mode = "triggered"
)

// SensorKind is what a device senses.
type SensorKind string

const (
	SensorContact    SensorKind = "contact"     // door or window contact; active when open
	SensorMotion     SensorKind = "motion"      // active on motion
	SensorSmoke      SensorKind = "smoke"       // active on smoke
	SensorVibration  SensorKind = "vibration"   // active on vibration
	SensorGlassBreak SensorKind = "glass_break" // active on glass break
	SensorPanic      SensorKind = "panic"       // active when the button is pressed
	SensorTamper     SensorKind = "tamper"      // active when the device is tampered with
)

// Origin says where an order came from.
type Origin string

const (
	OriginKeypad   Origin = "keypad"
	OriginApp      Origin = "app"
	OriginSchedule Origin = "schedule"
)

// CommandName is an order to the engine.
type CommandName string

const (
	CmdArm      CommandName = "arm"
	CmdArmNow   CommandName = "arm_now"
	CmdDisarm   CommandName = "disarm"
	CmdSilence  CommandName = "silence"
	CmdBypass   CommandName = "bypass"
	CmdPostpone CommandName = "postpone"
)

// AlertKind is the type of problem an alert reports.
type AlertKind string

const (
	AlertIntrusion  AlertKind = "intrusion"
	AlertFire       AlertKind = "fire"
	AlertPanic      AlertKind = "panic"
	AlertTamper     AlertKind = "tamper"
	AlertAuthFailed AlertKind = "auth_failed"
	// AlertArmingBypassed: the automatic arming went ahead with open zones, which
	// are therefore not protected.
	AlertArmingBypassed AlertKind = "arming_bypassed"
)

// Urgency ranks how fast someone must react to an alert.
type Urgency string

const (
	UrgencyHigh     Urgency = "high"
	UrgencyCritical Urgency = "critical"
)

// Status is the outcome of an order.
type Status string

const (
	StatusDone    Status = "done"
	StatusRefused Status = "refused"
	StatusExpired Status = "expired"
	StatusFailed  Status = "failed"
)

// Reason explains a refusal or a failure.
type Reason string

const (
	ReasonZoneOpen       Reason = "zone_open"
	ReasonBadCode        Reason = "bad_code"
	ReasonLockedOut      Reason = "locked_out"
	ReasonWrongState     Reason = "wrong_state"
	ReasonPostponeLimit  Reason = "postpone_limit"
	ReasonUnknownCommand Reason = "unknown_command"
)

// ClearedBy says how an alarm ended.
type ClearedBy string

const (
	ClearedByUser    ClearedBy = "user"
	ClearedByTimeout ClearedBy = "timeout"
)
