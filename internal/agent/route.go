package agent

import (
	"slices"
	"strings"

	"safehome/internal/contract"
)

// upRoute says whether a message of the hub goes to the cloud, and how. The list is
// the contract's "what goes to the cloud": alarm and alert messages, the alarm state,
// the health and the device list, the heartbeat, a few device events, the video
// sessions and status, the photos' announcements and the agent's own status. Raw
// sensor events and keypad events stay on the site.
type upRoute struct {
	types  []string // the envelope types accepted on this topic
	prio   Priority
	retain bool // a retained topic: also retained on the cloud's broker, only the latest counts
	// restate: the hub's latest state there is said again at each new connection to
	// the cloud (restate.go).
	restate bool
	device  bool // a device event: only some capabilities go up
	// own: only the agent publishes there, and hands its messages over itself (Own).
	// The hub's broker only gives them back, retained, at each new connection.
	own bool
}

// upCapabilities are the device events worth the cloud's attention. Never keypad: it
// carries the code.
var upCapabilities = []string{contract.CapSmoke, contract.CapPanic, contract.CapTamper, contract.CapMains, contract.CapBattery}

var alarmEvents = []string{
	contract.TypeAlarmArming, contract.TypeAlarmArmed, contract.TypeAlarmDisarmed, contract.TypeAlarmEntryDelay,
	contract.TypeAlarmTriggered, contract.TypeAlarmSirenCycle, contract.TypeAlarmSirenFailed, contract.TypeAlarmCleared,
	contract.TypeAlarmArmingScheduled, contract.TypeAlarmPostponed, contract.TypeAlarmAuthFailed,
}

// upRoutes is keyed by the topic without its root and site; "device/+/event" stands
// for any device.
var upRoutes = map[string]upRoute{
	"alarm/event":      {types: alarmEvents, prio: PriorityAlarm},
	"alarm/ack":        {types: []string{contract.TypeAlarmAck}, prio: PriorityAlarm},
	"alarm/state":      {types: []string{contract.TypeAlarmState}, prio: PriorityState, retain: true, restate: true},
	"alert/request":    {types: []string{contract.TypeAlertRequest}, prio: PriorityAlarm},
	"alert/result":     {types: []string{contract.TypeAlertResult}, prio: PriorityAlarm},
	"alert/ack":        {types: []string{contract.TypeAlertAck}, prio: PriorityAlarm},
	"system/health":    {types: []string{contract.TypeHealth}, prio: PriorityState, retain: true, restate: true},
	"system/devices":   {types: []string{contract.TypeDevices}, prio: PriorityState, retain: true, restate: true},
	"system/contacts":  {types: []string{contract.TypeContacts}, prio: PriorityState, retain: true, restate: true},
	"system/heartbeat": {types: []string{contract.TypeHeartbeat}, prio: PriorityState},
	"agent/status":     {types: []string{contract.TypeAgentStatus}, prio: PriorityState, retain: true, own: true},
	"device/+/event":   {types: []string{contract.TypeDeviceEvent}, prio: PriorityNormal, device: true},
	"video/session":    {types: []string{contract.TypeVideoSession}, prio: PriorityNormal},
	"video/status":     {types: []string{contract.TypeVideoStatus}, prio: PriorityState, retain: true},
	"config/applied":   {types: []string{contract.TypeConfigApplied}, prio: PriorityNormal, retain: true},
	"photo/taken":      {types: []string{contract.TypePhotoTaken}, prio: PriorityNormal},
	"photo/sent":       {types: []string{contract.TypePhotoSent}, prio: PriorityNormal},
	"maint/session":    {types: []string{contract.TypeMaintSession}, prio: PriorityNormal},
	"agent/replayed":   {types: []string{contract.TypeAgentReplayed}, prio: PriorityNormal, own: true},
	"agent/moved":      {types: []string{contract.TypeAgentMoved}, prio: PriorityNormal, own: true},
	// The owner sees in the app that someone opened the installation mode.
	"install/mode": {types: []string{contract.TypeInstallMode}, prio: PriorityNormal, retain: true},
}

// UpFilters lists the topics the agent reads on the hub's broker: not its own.
func UpFilters(site string) []string {
	var out []string
	for key, route := range upRoutes {
		if !route.own {
			out = append(out, contract.Topic(site, key))
		}
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

// downRoute is what the cloud may publish on a topic.
type downRoute struct {
	types []string // the envelope types accepted
	// code: the messages may carry the site's access code, which the cloud never
	// keeps. The agent reads them at QoS 0, so the cloud's broker queues none of them
	// for the agent while it is away, neither in memory nor in its file: an order
	// worth a minute is not kept for a day, even past its expiry. One the agent
	// misses is not run later; the phone sees it expired.
	code bool
}

// downRoutes are what the cloud may publish to the site, keyed like upRoutes. Anything
// else is refused: the cloud never commands a device nor pairs one. The tokens of
// video/cmd and photo/cmd are the cloud's own, made for one session or one photo: its
// broker may keep them, and a stop or an upload address still counts after an outage.
var downRoutes = map[string]downRoute{
	"alarm/cmd":    {types: []string{contract.TypeAlarmCommand}, code: true},
	"video/cmd":    {types: []string{contract.TypeVideoRequest, contract.TypeVideoStop}},
	"alert/result": {types: []string{contract.TypeAlertResult}},
	"alert/ack":    {types: []string{contract.TypeAlertAck}},
	"alert/seen":   {types: []string{contract.TypeAlertSeen}}, // the cloud's own: the hub never says it
	"config/set":   {types: []string{contract.TypeConfigSet}},
	"photo/cmd":    {types: []string{contract.TypePhotoUpload}}, // for the agent itself
	"maint/cmd":    {types: []string{contract.TypeMaintOpen, contract.TypeMaintClose}},
	"agent/cmd":    {types: []string{contract.TypeAgentReplay, contract.TypeAgentMove}}, // for the agent itself
}

// Filter is a topic the agent reads on the cloud's broker, with the QoS to ask for.
type Filter struct {
	Topic string
	QoS   byte
}

// DownFilters lists the topics the agent reads on the cloud's broker: at QoS 1, so
// the broker keeps what comes while the agent is away, except where the access code
// may pass (QoS 0, nothing kept).
func DownFilters(site string) []Filter {
	var out []Filter
	for key, route := range downRoutes {
		f := Filter{Topic: contract.Topic(site, key), QoS: 1}
		if route.code {
			f.QoS = 0
		}
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b Filter) int { return strings.Compare(a.Topic, b.Topic) })
	return out
}
