package agent

import (
	"slices"

	"safehome/internal/contract"
)

// upRoute says whether a message of the hub goes to the cloud, and how. The list is
// the contract's "what goes to the cloud": alarm and alert messages, the alarm state
// and the health, the heartbeat, a few device events, the video sessions and status,
// the photos' announcements and the agent's own status. Raw sensor events and keypad events stay on the site.
type upRoute struct {
	types  []string // the envelope types accepted on this topic
	prio   Priority
	retain bool // a retained topic: also retained on the cloud's broker, only the latest counts
	device bool // a device event: only some capabilities go up
}

// upCapabilities are the device events worth the cloud's attention. Never keypad: it
// carries the code.
var upCapabilities = []string{contract.CapSmoke, contract.CapPanic, contract.CapTamper, contract.CapMains, contract.CapBattery}

var alarmEvents = []string{
	contract.TypeAlarmArming, contract.TypeAlarmArmed, contract.TypeAlarmDisarmed, contract.TypeAlarmEntryDelay,
	contract.TypeAlarmTriggered, contract.TypeAlarmSirenCycle, contract.TypeAlarmCleared,
	contract.TypeAlarmArmingScheduled, contract.TypeAlarmPostponed, contract.TypeAlarmAuthFailed,
}

// upRoutes is keyed by the topic without its root and site; "device/+/event" stands
// for any device.
var upRoutes = map[string]upRoute{
	"alarm/event":      {types: alarmEvents, prio: PriorityAlarm},
	"alarm/ack":        {types: []string{contract.TypeAlarmAck}, prio: PriorityAlarm},
	"alarm/state":      {types: []string{contract.TypeAlarmState}, prio: PriorityState, retain: true},
	"alert/request":    {types: []string{contract.TypeAlertRequest}, prio: PriorityAlarm},
	"alert/result":     {types: []string{contract.TypeAlertResult}, prio: PriorityAlarm},
	"alert/ack":        {types: []string{contract.TypeAlertAck}, prio: PriorityAlarm},
	"system/health":    {types: []string{contract.TypeHealth}, prio: PriorityState, retain: true},
	"system/heartbeat": {types: []string{contract.TypeHeartbeat}, prio: PriorityState},
	"agent/status":     {types: []string{contract.TypeAgentStatus}, prio: PriorityState, retain: true},
	"device/+/event":   {types: []string{contract.TypeDeviceEvent}, prio: PriorityNormal, device: true},
	"video/session":    {types: []string{contract.TypeVideoSession}, prio: PriorityNormal},
	"video/status":     {types: []string{contract.TypeVideoStatus}, prio: PriorityState, retain: true},
	"config/applied":   {types: []string{contract.TypeConfigApplied}, prio: PriorityNormal, retain: true},
	"photo/taken":      {types: []string{contract.TypePhotoTaken}, prio: PriorityNormal},
	"photo/sent":       {types: []string{contract.TypePhotoSent}, prio: PriorityNormal},
	"maint/session":    {types: []string{contract.TypeMaintSession}, prio: PriorityNormal},
	"agent/replayed":   {types: []string{contract.TypeAgentReplayed}, prio: PriorityNormal},
	// The owner sees in the app that someone opened the installation mode.
	"install/mode": {types: []string{contract.TypeInstallMode}, prio: PriorityNormal, retain: true},
}

// UpFilters lists the topics the agent reads on the hub's broker.
func UpFilters(site string) []string {
	var out []string
	for key := range upRoutes {
		out = append(out, contract.Topic(site, key))
	}
	slices.Sort(out)
	return out
}

// routeKey turns topic parts (after the site) into a key of upRoutes.
func routeKey(parts []string) string {
	if len(parts) == 3 && parts[0] == "device" && parts[2] == "event" {
		return "device/+/event"
	}
	if len(parts) != 2 {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// downRoutes are what the cloud may publish to the site, keyed like upRoutes, with
// the envelope types accepted. Anything else is refused: the cloud never commands a
// device nor pairs one.
var downRoutes = map[string][]string{
	"alarm/cmd":    {contract.TypeAlarmCommand},
	"video/cmd":    {contract.TypeVideoRequest, contract.TypeVideoStop},
	"alert/result": {contract.TypeAlertResult},
	"alert/ack":    {contract.TypeAlertAck},
	"config/set":   {contract.TypeConfigSet},
	"photo/cmd":    {contract.TypePhotoUpload}, // for the agent itself
	"maint/cmd":    {contract.TypeMaintOpen, contract.TypeMaintClose},
	"agent/cmd":    {contract.TypeAgentReplay}, // for the agent itself
}

// DownFilters lists the topics the agent reads on the cloud's broker.
func DownFilters(site string) []string {
	var out []string
	for key := range downRoutes {
		out = append(out, contract.Topic(site, key))
	}
	slices.Sort(out)
	return out
}
