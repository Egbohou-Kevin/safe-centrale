package contract

import (
	"slices"
	"testing"
)

func TestTopics(t *testing.T) {
	cases := map[string]string{
		DeviceEventTopic("tg-lom-0001", "dev_k7p2"):             "safehome/tg-lom-0001/device/dev_k7p2/event",
		DeviceCmdTopic("tg-lom-0001", "dev_s3x9"):               "safehome/tg-lom-0001/device/dev_s3x9/cmd",
		AlarmCmdTopic("tg-lom-0001"):                            "safehome/tg-lom-0001/alarm/cmd",
		AlarmStateTopic("tg-lom-0001"):                          "safehome/tg-lom-0001/alarm/state",
		DeviceStateTopic("tg-lom-0001", "dev_k7p2", CapBattery): "safehome/tg-lom-0001/device/dev_k7p2/state/battery",
		AlertRequestTopic("tg-lom-0001"):                        "safehome/tg-lom-0001/alert/request",
		HeartbeatTopic("tg-lom-0001"):                           "safehome/tg-lom-0001/system/heartbeat",
		DevicesTopic("tg-lom-0001"):                             "safehome/tg-lom-0001/system/devices",
		AgentCmdTopic("tg-lom-0001"):                            "safehome/tg-lom-0001/agent/cmd",
		AgentReplayedTopic("tg-lom-0001"):                       "safehome/tg-lom-0001/agent/replayed",
		InstallModeTopic("tg-lom-0001"):                         "safehome/tg-lom-0001/install/mode",
		InstallCmdTopic("tg-lom-0001"):                          "safehome/tg-lom-0001/install/cmd",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("topic %q, want %q", got, want)
		}
	}
}

func TestParseTopic(t *testing.T) {
	good := map[string]struct {
		site  string
		parts []string
	}{
		"safehome/tg-lom-0001/alarm/cmd":             {"tg-lom-0001", []string{"alarm", "cmd"}},
		"safehome/tg-lom-0001/device/dev_k7p2/event": {"tg-lom-0001", []string{"device", "dev_k7p2", "event"}},
		"safehome/tg-lom-0001/system/health":         {"tg-lom-0001", []string{"system", "health"}},
	}
	for topic, want := range good {
		site, parts, ok := ParseTopic(topic)
		if !ok || site != want.site || !slices.Equal(parts, want.parts) {
			t.Errorf("%s: %q %v %v", topic, site, parts, ok)
		}
	}
	for _, topic := range []string{"", "safehome", "safehome/", "safehome/tg-lom-0001", "safehome/tg-lom-0001/", "safehome/TG/alarm/cmd",
		"other/tg-lom-0001/alarm/cmd", "safehome/tg-lom-0001//cmd", "safehome//alarm/cmd", "safehome/tg-lom-0001/alarm/", "/safehome/tg-lom-0001/alarm/cmd"} {
		if _, _, ok := ParseTopic(topic); ok {
			t.Errorf("%q must be refused", topic)
		}
	}
	site, parts, ok := ParseTopic(DeviceCmdTopic("tg-lom-0001", "dev_s3x9"))
	if !ok || site != "tg-lom-0001" || !slices.Equal(parts, []string{"device", "dev_s3x9", "cmd"}) {
		t.Fatalf("%q %v %v", site, parts, ok)
	}
}

func TestRegistryAndZigbeeTopics(t *testing.T) {
	for got, want := range map[string]string{
		RegistryCmdTopic("s"):     "safehome/s/registry/cmd",
		RegistryEventTopic("s"):   "safehome/s/registry/event",
		RegistryDevicesTopic("s"): "safehome/s/registry/devices",
		ZigbeeStatusTopic("s"):    "safehome/s/zigbee/status",
	} {
		if got != want {
			t.Errorf("%s, want %s", got, want)
		}
	}
}

func TestVideoTopics(t *testing.T) {
	for got, want := range map[string]string{
		VideoCmdTopic("s"):     "safehome/s/video/cmd",
		VideoSessionTopic("s"): "safehome/s/video/session",
		VideoStatusTopic("s"):  "safehome/s/video/status",
	} {
		if got != want {
			t.Errorf("%s, want %s", got, want)
		}
	}
}
