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
// mosquitto.conf and safehome.acl back (the cases themselves are in update_hub_test.go).

// What the journal says of a client the hub's broker refuses: one of our programs
// (internal/adapter/mqtt), Zigbee2MQTT as mqtt.js says it, with MQTT 3.1.1 and 5.
const (
	hubRefused     = `Oct 29 15:00:02 hub safehome-hub[44]: level=ERROR msg="mqtt broker refused the connection" reason=135 err="server denied connect (reason: 135): failed to connect to server: "`
	z2mRefused     = "Oct 29 15:00:02 hub zigbee2mqtt[45]: [2026-10-29 15:00:02] error: \tz2m: MQTT error: Connection refused: Not authorized"
	z2mBadPassword = "Oct 29 15:00:02 hub zigbee2mqtt[45]: [2026-10-29 15:00:02] error: \tz2m: MQTT error: Connection refused: Bad User Name or Password"
	// What the ACL refuses (29 October 2026): a subscription (a broker that says so; Mosquitto
	// 2.1.2 grants it and delivers nothing), a message the program may not write.
	subscriptionRefused = `Oct 29 15:00:02 hub safehome-video[46]: level=ERROR msg="mqtt subscription refused" filter=safehome/tg-lom-0001/video/cmd reason=135`
	messageRefused      = `Oct 29 15:00:02 hub safehome-gsm[47]: level=ERROR msg="the broker refused a message" topic=safehome/tg-lom-0001/gsm/status type=gsm.status err="mqtt: broker refused the message: safehome/tg-lom-0001/gsm/status (reason 0x87)" attempt=1`
)

// update-hub.sh looks for the words our MQTT client and outbox write when the broker
// refuses them: they must stay the same on both sides.
func TestUpdateHubLooksForTheRefusalTheClientWrites(t *testing.T) {
	script := string(must(os.ReadFile("update-hub.sh")))
	start := strings.Index(script, "\nrefusal='")
	if start < 0 {
		t.Fatal("update-hub.sh has no refusal='...'")
	}
	line := script[start+len("\nrefusal='"):]
	alternatives := strings.Split(line[:strings.Index(line, "'")], "|")
	for i, c := range []struct{ file, said string }{
		{filepath.Join("..", "internal", "adapter", "mqtt", "client.go"), "mqtt broker refused the connection"},
		{filepath.Join("..", "internal", "adapter", "mqtt", "client.go"), "mqtt subscription refused"},
		{filepath.Join("..", "internal", "outbox", "outbox.go"), "the broker refused a message"},
	} {
		if !strings.Contains(string(must(os.ReadFile(c.file))), `"`+c.said+`"`) || i >= len(alternatives) || alternatives[i] != c.said {
			t.Errorf("%s logs %q, and update-hub.sh's refusal says it, alternative %d: one of them changed", c.file, c.said, i+1)
		}
	}
}

// checkBrokerBack checks mosquitto.conf and safehome.acl after phase 2, install.sh having
// written broker and acl in them, the ACL aclBefore before (empty: none): back as they
// were when the rollback ran, an ACL the hub had not left (the mosquitto.conf back does
// not name it), mosquitto then restarted once, after daemon-reload and before any of the
// programs restarted. It returns where mosquitto restarted, -1 if it did not.
func checkBrokerBack(t *testing.T, dir string, cmds []string, broker, acl, aclBefore string, holds bool, reload int, restarted map[string]int) int {
	t.Helper()
	conf := "allow_anonymous true"
	if holds && broker != "" {
		conf = broker
	}
	if got := strings.TrimSpace(string(must(os.ReadFile(filepath.Join(dir, "hub", "etc", "mosquitto", "mosquitto.conf"))))); got != conf {
		t.Errorf("mosquitto.conf = %q, want %q", got, conf)
	}
	wantACL := aclBefore
	if acl != "" && (holds || aclBefore == "") {
		wantACL = acl
	}
	gotACL, err := os.ReadFile(filepath.Join(dir, "hub", "etc", "mosquitto", "safehome.acl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(gotACL)); got != wantACL {
		t.Errorf("safehome.acl = %q, want %q", got, wantACL)
	}
	const mosquitto = "sudo -n systemctl restart mosquitto.service"
	want := (broker != "" || (acl != "" && aclBefore != "")) && !holds
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

// holdCase is a case of TestUpdateHubPhaseTwoPutsThePreviousProgramsBackWhenTheNewOnesDoNotHold.
type holdCase struct {
	name, installed, crashing, flapping, flapAge, stopped, mute, install, mvFails, restart, markFails, markMode string
	refused, refusal, journalFails                                                                              string
	noPrev, marked                                                                                              bool
	state                                                                                                       string
	want                                                                                                        []string
}

// refusalCases are the cases of a program the broker refuses: it stays active, deaf to
// every sensor (29 October 2026); back is the state of the programs put back.
func refusalCases(back string) []holdCase {
	return []holdCase{
		{name: "the broker refuses a new program", installed: "0.3.0", refused: "0.3.0", refusal: hubRefused, marked: true, state: back,
			want: []string{"!! the broker refuses a program:", "safehome-hub[44]", "the new programs do not hold; the previous programs are back and their services hold"}},
		{name: "the broker refuses Zigbee2MQTT", installed: "0.3.0", refused: "0.3.0", refusal: z2mRefused, marked: true, state: back,
			want: []string{"!! the broker refuses a program:", "zigbee2mqtt[45]", "the new programs do not hold; the previous programs are back and their services hold"}},
		{name: "the broker refuses a bad user name or password", installed: "0.3.0", refused: "0.3.0", refusal: z2mBadPassword, marked: true, state: back,
			want: []string{"!! the broker refuses a program:", "the new programs do not hold; the previous programs are back and their services hold"}},
		// A topic the ACL does not grant (29 October 2026).
		{name: "the broker refuses a subscription", installed: "0.3.0", refused: "0.3.0", refusal: subscriptionRefused, marked: true, state: back,
			want: []string{"!! the broker refuses a program:", "safehome-video[46]", "the new programs do not hold; the previous programs are back and their services hold"}},
		{name: "the broker refuses a message", installed: "0.3.0", refused: "0.3.0", refusal: messageRefused, marked: true, state: back,
			want: []string{"!! the broker refuses a program:", "safehome-gsm[47]", "the new programs do not hold; the previous programs are back and their services hold"}},
		{name: "the broker refuses the previous programs too", installed: "0.3.0", refused: "0.3.0 0.2.1", refusal: hubRefused, marked: true, state: back,
			want: []string{"the new programs do not hold; the previous programs are back but the services still do not hold"}},
	}
}

// unitsCase is a case of TestUpdateHubPhaseTwoPutsTheSavedUnitsBack.
type unitsCase struct {
	name, newUnit, extra, tar, install, hubUnit string
	changed, changedAs, broker, acl, aclBefore  string
	noPrev, extraLeft, hubStays                 bool
	want                                        []string
}

// brokerUnitsCases are the cases of the broker's configuration and ACL put back;
// tight is a unit that makes the hub fail, closedBroker a new mosquitto.conf.
func brokerUnitsCases(tight, closedBroker string) []unitsCase {
	return []unitsCase{
		// Programs from before 29 October 2026 have no password: the broker that refuses
		// anonymous clients goes back with them, and restarts before them.
		{name: "a new broker configuration goes back with the programs", newUnit: tight, hubUnit: "[Unit]", broker: closedBroker,
			want: []string{"   unit safehome-hub.service put back as it was", "   mosquitto.conf put back as it was",
				"   mosquitto restarted on its previous configuration",
				"the new programs do not hold; the previous programs and units are back and their services hold"}},
		{name: "a new broker configuration alone goes back when install.sh fails", noPrev: true, install: "before", hubUnit: "[Unit]", hubStays: true,
			broker: closedBroker,
			want: []string{"   mosquitto.conf put back as it was", "   mosquitto restarted on its previous configuration",
				"install.sh failed; the previous units are back and their services hold"}},
		{name: "a new broker configuration that holds stays", newUnit: "[Unit] new", hubUnit: "[Unit] new", broker: closedBroker,
			want: []string{"phase 2 done"}},
		// The previous programs may use a topic the new ACL no longer grants: the ACL goes
		// back with them, and mosquitto restarts on it.
		{name: "a new ACL goes back with the broker configuration", newUnit: tight, hubUnit: "[Unit]", broker: closedBroker,
			aclBefore: "user old", acl: "user new",
			want: []string{"   safehome.acl put back as it was", "   mosquitto.conf put back as it was",
				"   mosquitto restarted on its previous configuration",
				"the new programs do not hold; the previous programs and units are back and their services hold"}},
		{name: "a new ACL alone goes back", newUnit: tight, hubUnit: "[Unit]", aclBefore: "user old", acl: "user new",
			want: []string{"   safehome.acl put back as it was", "   mosquitto restarted on its previous configuration",
				"the new programs do not hold; the previous programs and units are back and their services hold"}},
		{name: "the first ACL stays when the broker configuration goes back", newUnit: tight, hubUnit: "[Unit]", broker: closedBroker,
			acl: "user new",
			want: []string{"   mosquitto.conf put back as it was", "   mosquitto restarted on its previous configuration",
				"the new programs do not hold; the previous programs and units are back and their services hold"}},
		{name: "a new ACL that holds stays", newUnit: "[Unit] new", hubUnit: "[Unit] new", broker: closedBroker,
			aclBefore: "user old", acl: "user new",
			want: []string{"phase 2 done"}},
	}
}
