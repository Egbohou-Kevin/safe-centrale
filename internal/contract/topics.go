package contract

import "strings"

// TopicRoot is the first level of every topic of the contract.
const TopicRoot = "safehome"

// Topic builds an MQTT topic: safehome/{site}/{parts...}.
func Topic(site string, parts ...string) string {
	return TopicRoot + "/" + site + "/" + strings.Join(parts, "/")
}

// ParseTopic splits "safehome/{site}/a/b/c" into the site and the parts after it
// (["a", "b", "c"]). ok is false for any topic that is not one of ours, including
// one with an empty part.
func ParseTopic(topic string) (site string, parts []string, ok bool) {
	rest, found := strings.CutPrefix(topic, TopicRoot+"/")
	if !found {
		return "", nil, false
	}
	fields := strings.Split(rest, "/")
	if len(fields) < 2 || !sitePattern.MatchString(fields[0]) {
		return "", nil, false
	}
	for _, f := range fields[1:] {
		if f == "" {
			return "", nil, false
		}
	}
	return fields[0], fields[1:], true
}

func DeviceEventTopic(site, deviceID string) string { return Topic(site, "device", deviceID, "event") }

// DeviceStateTopic carries the retained state of one capability of a device: one
// topic per capability, because the broker keeps only the last retained message of
// a topic (a battery reading must not erase the door's state).
func DeviceStateTopic(site, deviceID, capability string) string {
	return Topic(site, "device", deviceID, "state", capability)
}

func DeviceCmdTopic(site, deviceID string) string { return Topic(site, "device", deviceID, "cmd") }
func DeviceAckTopic(site, deviceID string) string { return Topic(site, "device", deviceID, "ack") }

func AlarmEventTopic(site string) string { return Topic(site, "alarm", "event") }
func AlarmStateTopic(site string) string { return Topic(site, "alarm", "state") }
func AlarmCmdTopic(site string) string   { return Topic(site, "alarm", "cmd") }
func AlarmAckTopic(site string) string   { return Topic(site, "alarm", "ack") }

func AlertRequestTopic(site string) string { return Topic(site, "alert", "request") }
func AlertResultTopic(site string) string  { return Topic(site, "alert", "result") }
func AlertAckTopic(site string) string     { return Topic(site, "alert", "ack") }
func AlertSeenTopic(site string) string    { return Topic(site, "alert", "seen") }

func HeartbeatTopic(site string) string { return Topic(site, "system", "heartbeat") }
func HealthTopic(site string) string    { return Topic(site, "system", "health") }

// DevicesTopic carries the health of each device (system.devices), retained.
func DevicesTopic(site string) string { return Topic(site, "system", "devices") }

// ContactsTopic carries who an intrusion tells and when (system.contacts), retained.
func ContactsTopic(site string) string { return Topic(site, "system", "contacts") }

// GSMStatusTopic carries the GSM gateway's lines, retained.
func GSMStatusTopic(site string) string { return Topic(site, "gsm", "status") }

// The device registry, kept by the Zigbee adapter: orders (local only), what
// happened, and the retained list.
func RegistryCmdTopic(site string) string     { return Topic(site, "registry", "cmd") }
func RegistryEventTopic(site string) string   { return Topic(site, "registry", "event") }
func RegistryDevicesTopic(site string) string { return Topic(site, "registry", "devices") }

// AgentStatusTopic carries the state of the link to the cloud, retained.
func AgentStatusTopic(site string) string { return Topic(site, "agent", "status") }

// Orders from the cloud for the agent itself (agent.replay, agent.move), and its
// answers.
func AgentCmdTopic(site string) string      { return Topic(site, "agent", "cmd") }
func AgentReplayedTopic(site string) string { return Topic(site, "agent", "replayed") }
func AgentMovedTopic(site string) string    { return Topic(site, "agent", "moved") }

// InstallModeTopic carries the installation mode, retained; InstallCmdTopic the
// local console's orders to the hub (never the cloud's).
func InstallModeTopic(site string) string { return Topic(site, "install", "mode") }
func InstallCmdTopic(site string) string  { return Topic(site, "install", "cmd") }

// ZigbeeStatusTopic carries the state of the Zigbee network, retained.
func ZigbeeStatusTopic(site string) string { return Topic(site, "zigbee", "status") }

// Live video: orders from the cloud (request, stop), how sessions go, and the
// retained state of the video service.
func VideoCmdTopic(site string) string     { return Topic(site, "video", "cmd") }
func VideoSessionTopic(site string) string { return Topic(site, "video", "session") }
func VideoStatusTopic(site string) string  { return Topic(site, "video", "status") }
func PhotoTakenTopic(site string) string   { return Topic(site, "photo", "taken") }
func PhotoCmdTopic(site string) string     { return Topic(site, "photo", "cmd") }
func PhotoSentTopic(site string) string    { return Topic(site, "photo", "sent") }
func MaintCmdTopic(site string) string     { return Topic(site, "maint", "cmd") }
func MaintSessionTopic(site string) string { return Topic(site, "maint", "session") }
