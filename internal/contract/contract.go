// Package contract implements the safeHome event contract (see docs/contract.md):
// envelope, message types, payloads and topics. It is the published language that
// every component and the cloud speak; it knows nothing about the alarm domain.
package contract

// Version is the contract version implemented by this package.
const Version = 1

// Message types.
const (
	TypeDeviceEvent = "device.event"
	TypeDeviceState = "device.state"
	TypeCommand     = "command"
	TypeCommandAck  = "command.ack"

	TypeAlarmCommand         = "alarm.command"
	TypeAlarmAck             = "alarm.ack"
	TypeAlarmArming          = "alarm.arming"
	TypeAlarmArmed           = "alarm.armed"
	TypeAlarmDisarmed        = "alarm.disarmed"
	TypeAlarmEntryDelay      = "alarm.entry_delay"
	TypeAlarmTriggered       = "alarm.triggered"
	TypeAlarmSirenCycle      = "alarm.siren_cycle"
	TypeAlarmSirenFailed     = "alarm.siren_failed"
	TypeAlarmCleared         = "alarm.cleared"
	TypeAlarmArmingScheduled = "alarm.arming_scheduled"
	TypeAlarmPostponed       = "alarm.postponed"
	TypeAlarmAuthFailed      = "alarm.auth_failed"
	TypeAlarmState           = "alarm.state"

	TypeAlertRequest = "alert.request"
	TypeAlertResult  = "alert.result"
	TypeAlertAck     = "alert.ack"

	TypeRegistryPair           = "registry.pair"
	TypeRegistryRemove         = "registry.remove"
	TypeRegistrySnapshot       = "registry.snapshot"
	TypeRegistryDeviceAdded    = "registry.device_added"
	TypeRegistryDeviceRejected = "registry.device_rejected"
	TypeRegistryDeviceRemoved  = "registry.device_removed"
	TypeRegistryPairFailed     = "registry.pair_failed"

	TypeHeartbeat = "system.heartbeat"
	TypeHealth    = "system.health"
	TypeDevices   = "system.devices"

	TypeGSMStatus    = "gsm.status"
	TypeZigbeeStatus = "zigbee.status"
	TypeAgentStatus  = "agent.status"

	TypeAgentReplay   = "agent.replay"
	TypeAgentReplayed = "agent.replayed"

	TypeInstallMode  = "install.mode"
	TypeInstallClose = "install.close"

	TypeVideoRequest = "video.request"
	TypeVideoStop    = "video.stop"
	TypeVideoSession = "video.session"
	TypeVideoStatus  = "video.status"

	TypePhotoTaken  = "photo.taken"
	TypePhotoUpload = "photo.upload"
	TypePhotoSent   = "photo.sent"

	TypeMaintOpen    = "maint.open"
	TypeMaintClose   = "maint.close"
	TypeMaintSession = "maint.session"
)

// AdapterZigbee names the Zigbee adapter: the source of its messages, and the
// adapter of the devices it registers.
const AdapterZigbee = "adapter.zigbee"

// Why a device was refused at pairing (registry.device_rejected).
const (
	RejectUnknownModel    = "unknown_model"    // no profile for this model
	RejectNotPairing      = "not_pairing"      // it joined outside a pairing window
	RejectInterviewFailed = "interview_failed" // its model could not be read
)

// AgentLink names the link agent: the source of its messages.
const AgentLink = "agent.link"

// The link to the cloud in agent.status.
const (
	UplinkUp   = "up"
	UplinkDown = "down"
)

// Bridge states in zigbee.status.
const (
	BridgeOnline  = "online"
	BridgeOffline = "offline"
)

// Device capabilities.
const (
	CapContact      = "contact"
	CapMotion       = "motion"
	CapSmoke        = "smoke"
	CapVibration    = "vibration"
	CapGlassBreak   = "glass_break"
	CapPanic        = "panic"
	CapTamper       = "tamper"
	CapKeypad       = "keypad"
	CapSiren        = "siren"
	CapMains        = "mains"
	CapBattery      = "battery"
	CapLink         = "link"
	CapAvailability = "availability"
)

// Capability values.
const (
	ValOpen      = "open"
	ValClosed    = "closed"
	ValDetected  = "detected"
	ValClear     = "clear"
	ValAlarm     = "alarm"
	ValPressed   = "pressed"
	ValTriggered = "triggered"
	ValOn        = "on"
	ValOff       = "off"
	ValPresent   = "present"
	ValLost      = "lost"
	ValOnline    = "online"
	ValOffline   = "offline"

	ValArm      = "arm"
	ValArmNow   = "arm_now"
	ValDisarm   = "disarm"
	ValSilence  = "silence"
	ValPostpone = "postpone"
)

// Commands sent to the alarm engine (topic alarm/cmd).
const (
	CmdArm      = "alarm.arm"
	CmdArmNow   = "alarm.arm_now"
	CmdDisarm   = "alarm.disarm"
	CmdSilence  = "alarm.silence"
	CmdEnd      = "alarm.end"
	CmdBypass   = "alarm.bypass"
	CmdPostpone = "alarm.postpone"
)

// Commands sent to devices.
const (
	CmdSirenOn        = "siren.on"
	CmdSirenOff       = "siren.off"
	CmdDeviceIdentify = "device.identify"
)

// Origins of an order or of an alarm event.
const (
	OriginKeypad   = "keypad"
	OriginApp      = "app"
	OriginSchedule = "schedule"
	OriginUser     = "user"
	OriginTimeout  = "timeout"
)

// Alarm states.
const (
	StateDisarmed   = "disarmed"
	StateArming     = "arming"
	StateArmed      = "armed"
	StateEntryDelay = "entry_delay"
	StateTriggered  = "triggered"
)

// Siren schedule of a triggered alarm (siren of alarm.state).
const (
	SirenOn     = "on"     // in a siren cycle
	SirenPaused = "paused" // in the pause between two cycles
)

// Acknowledgement statuses (alarm.ack).
const (
	AckDone    = "done"
	AckRefused = "refused"
	AckExpired = "expired"
	AckFailed  = "failed"
)

// Reasons for a refusal or a failure.
const (
	ReasonZoneOpen       = "zone_open"
	ReasonBadCode        = "bad_code"
	ReasonLockedOut      = "locked_out"
	ReasonWrongState     = "wrong_state"
	ReasonPostponeLimit  = "postpone_limit"
	ReasonUnknownCommand = "unknown_command"
	// ReasonRateLimited: the link agent refused an order from the app over its limit
	// of messages from the cloud; the hub never got it.
	ReasonRateLimited = "rate_limited"
)

// Alert kinds (alert.request).
const (
	AlertIntrusion      = "intrusion"
	AlertFire           = "fire"
	AlertPanic          = "panic"
	AlertTamper         = "tamper"
	AlertAuthFailed     = "auth_failed"
	AlertArmingBypassed = "arming_bypassed"
	AlertBatteryLowHub  = "battery_low_hub"
	AlertMainsRestored  = "mains_restored"
	AlertDeviceOffline  = "device_offline"
	AlertBatteryLow     = "battery_low"
	AlertGSMDegraded    = "gsm_degraded"
	AlertZigbeeDown     = "zigbee_down"
	AlertSirenFailed    = "siren_failed"
)

// Alert urgency.
const (
	UrgencyCritical = "critical"
	UrgencyHigh     = "high"
	UrgencyLow      = "low"
)

// Alert channels.
const (
	ChannelApp      = "app"
	ChannelWhatsApp = "whatsapp"
	ChannelSMS      = "sms"
	ChannelCall     = "call"
)

// Alert delivery statuses (alert.result).
const (
	ResultQueued    = "queued"
	ResultSent      = "sent"
	ResultDelivered = "delivered"
	ResultFailed    = "failed"
)

// ReasonNoUplink: an alert for a cloud channel (app, WhatsApp) could not reach the
// cloud, or the cloud did not confirm it in time. The escalation turns to the GSM.
const ReasonNoUplink = "no_uplink"

// IsActive reports whether a capability value is an activation, that is, what can
// trip a zone: door open, motion, smoke, and so on.
func IsActive(capability, value string) bool {
	switch capability {
	case CapContact:
		return value == ValOpen
	case CapMotion, CapVibration, CapGlassBreak:
		return value == ValDetected
	case CapSmoke:
		return value == ValAlarm
	case CapPanic:
		return value == ValPressed
	case CapTamper:
		return value == ValTriggered
	}
	return false
}

// allowedValues lists the permitted values of enumerated capabilities.
var allowedValues = map[string][]string{
	CapContact:      {ValOpen, ValClosed},
	CapMotion:       {ValDetected, ValClear},
	CapSmoke:        {ValAlarm, ValClear},
	CapVibration:    {ValDetected, ValClear},
	CapGlassBreak:   {ValDetected, ValClear},
	CapPanic:        {ValPressed},
	CapTamper:       {ValTriggered, ValClear},
	CapKeypad:       {ValArm, ValArmNow, ValDisarm, ValSilence, ValPostpone},
	CapSiren:        {ValOn, ValOff},
	CapMains:        {ValPresent, ValLost},
	CapAvailability: {ValOnline, ValOffline},
}

// numericCapabilities are the capabilities whose value is an integer from 0 to 100.
var numericCapabilities = map[string]bool{CapBattery: true, CapLink: true}
