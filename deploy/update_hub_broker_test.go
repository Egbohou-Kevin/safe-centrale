package deploy

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// update-hub.sh and the hub's broker, which takes no anonymous client since
// 29 October 2026: a program it refuses does not hold, and a rollback puts the previous
// mosquitto.conf back (the cases themselves are in update_hub_test.go).

// What the journal says of a client the hub's broker refuses: one of our programs
// (internal/adapter/mqtt), Zigbee2MQTT as mqtt.js says it, with MQTT 3.1.1 and 5.
const (
	hubRefused     = `Oct 29 15:00:02 hub safehome-hub[44]: level=ERROR msg="mqtt broker refused the connection" reason=135 err="server denied connect (reason: 135): failed to connect to server: "`
	z2mRefused     = "Oct 29 15:00:02 hub zigbee2mqtt[45]: [2026-10-29 15:00:02] error: \tz2m: MQTT error: Connection refused: Not authorized"
	z2mBadPassword = "Oct 29 15:00:02 hub zigbee2mqtt[45]: [2026-10-29 15:00:02] error: \tz2m: MQTT error: Connection refused: Bad User Name or Password"
)

// update-hub.sh looks for the words our MQTT client writes when the broker refuses it:
// they must stay the same on both sides.
func TestUpdateHubLooksForTheRefusalTheClientWrites(t *testing.T) {
	const said = `"mqtt broker refused the connection"`
	client := string(must(os.ReadFile(filepath.Join("..", "internal", "adapter", "mqtt", "client.go"))))
	script := string(must(os.ReadFile("update-hub.sh")))
	if !strings.Contains(client, said) || !strings.Contains(script, "refusal='"+strings.Trim(said, `"`)+"|") {
		t.Fatalf("internal/adapter/mqtt/client.go logs %s, and update-hub.sh's refusal starts with it: one of them changed", said)
	}
}

// checkBrokerBack checks mosquitto.conf after phase 2, install.sh having written broker
// in it: back as it was when the rollback ran, mosquitto then restarted once, after
// daemon-reload and before any of the programs restarted. It returns where mosquitto
// restarted, -1 if it did not.
func checkBrokerBack(t *testing.T, dir string, cmds []string, broker string, holds bool, reload int, restarted map[string]int) int {
	t.Helper()
	conf := "allow_anonymous true"
	if holds && broker != "" {
		conf = broker
	}
	if got := strings.TrimSpace(string(must(os.ReadFile(filepath.Join(dir, "hub", "etc", "mosquitto", "mosquitto.conf"))))); got != conf {
		t.Errorf("mosquitto.conf = %q, want %q", got, conf)
	}
	const mosquitto = "sudo -n systemctl restart mosquitto.service"
	want := broker != "" && !holds
	at := slices.Index(cmds, mosquitto)
	if (at >= 0) != want || strings.Count(strings.Join(cmds, "\n"), mosquitto) > 1 {
		t.Errorf("mosquitto restarted at %d, want it restarted once: %v", at, want)
		return at
	}
	for unit, i := range restarted {
		if want && (i < at || at < reload) {
			t.Errorf("daemon-reload at %d, mosquitto restarted at %d, %s at %d: want them in this order", reload, at, unit, i)
		}
	}
	return at
}
