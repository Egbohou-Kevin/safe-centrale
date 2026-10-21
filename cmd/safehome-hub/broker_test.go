package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"safehome/internal/adapter/mqtt/mqtttest"
	"safehome/internal/adapter/pidfile"
	"safehome/internal/contract"
)

func TestTheHubEndToEndOverARealBroker(t *testing.T) {
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state, "")
	if code, _, errb := cli(t, "482915\n", "codes", "-config", cfg, "set", "u_owner"); code != 0 {
		t.Fatal(errb)
	}
	l := startLive(t, broker, state, cfg)

	// Boot: the retained state and health appear.
	l.waitState("disarmed")
	l.waitCount(contract.TypeHealth, 1)

	// Arm from the keypad, then force the back door.
	l.event("dev_keypad", contract.CapKeypad, "arm", "")
	l.waitState("arming")
	l.event("dev_keypad", contract.CapKeypad, "arm_now", "")
	l.waitState("armed")
	l.event("dev_back", contract.CapContact, "open", "")
	l.waitCount(contract.TypeAlarmTriggered, 1)
	l.waitFor("the siren order", func() bool { return len(l.siren()) == 1 })
	l.waitCount(contract.TypeAlertRequest, 1)
	alert, _ := contract.DecodeData[contract.AlertRequestData](l.seen(contract.TypeAlertRequest)[0])
	if alert.Kind != contract.AlertIntrusion || alert.Contact != "u_owner" || alert.Channel != contract.ChannelApp {
		t.Fatalf("%+v", alert)
	}
	if s := l.siren()[0]; s.DeviceID != "dev_siren1" || s.Command != contract.CmdSirenOn || s.Args.DurationS != 300 {
		t.Fatalf("%+v", s)
	}
	l.waitState("triggered")

	// A wrong code, then the right one.
	l.event("dev_keypad", contract.CapKeypad, "disarm", "111122")
	l.waitCount(contract.TypeAlarmAuthFailed, 1)
	l.event("dev_keypad", contract.CapKeypad, "disarm", "482915")
	l.waitCount(contract.TypeAlarmDisarmed, 1)
	l.waitState("disarmed")
	l.waitFor("the siren to be switched off", func() bool { return len(l.siren()) == 2 })
	if d, _ := contract.DecodeData[contract.DisarmedData](l.seen(contract.TypeAlarmDisarmed)[0]); d.User != "u_owner" || d.Origin != contract.OriginKeypad {
		t.Fatalf("%+v", d)
	}

	// A late subscriber still learns the state, from the retained message.
	late := &live{t: t, got: make(chan struct{}, 16)}
	dialTestClient(t, broker.URL(), late)
	late.waitFor("the retained state", func() bool { return late.lastState() == "disarmed" })

	l.validateEverything("482915", "111122")

	if code := l.stopHub(); code != 0 {
		t.Fatalf("exit code %d\n%s", code, l.logs.String())
	}
	if _, err := os.Stat(filepath.Join(state, pidName)); !os.IsNotExist(err) {
		t.Fatal("the pid file must be removed at shutdown")
	}
}

func TestTheStateSurvivesARestart(t *testing.T) {
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state, "")
	l := startLive(t, broker, state, cfg)
	l.waitState("disarmed")
	l.event("dev_keypad", contract.CapKeypad, "arm", "")
	l.waitState("arming")
	l.event("dev_keypad", contract.CapKeypad, "arm_now", "")
	l.waitState("armed")
	seqBefore := lastSeq(l)
	if code := l.stopHub(); code != 0 {
		t.Fatal(code)
	}

	// The power cut and came back: a new process on the same disk.
	l.startHub()
	l.waitFor("the hub to come back", func() bool { return lastSeq(l) > seqBefore })
	l.event("dev_back", contract.CapContact, "open", "")
	l.waitCount(contract.TypeAlarmTriggered, 1) // only an armed alarm reacts: it came back armed
	l.waitFor("the siren", func() bool { return len(l.siren()) == 1 })

	// The message counter went on where it stopped.
	var prev uint64
	for _, m := range l.allFromHub() {
		env, _ := contract.ParseEnvelope(m.Payload)
		if env.Seq <= prev {
			t.Fatalf("seq %d after %d: the counter must only go up, restarts included", env.Seq, prev)
		}
		prev = env.Seq
	}
	l.validateEverything()
}

func TestTheHubLearnsOpenDoorsFromRetainedStatesAtBoot(t *testing.T) {
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state, "")

	// Before the hub starts, the broker already holds the back door's state: open.
	l := &live{t: t, broker: broker, cfgPath: cfg, state: state, logs: &syncBuffer{}, ids: contract.NewIDGenerator(), got: make(chan struct{}, 1024)}
	l.client = dialTestClient(t, broker.URL(), l)
	l.publish(contract.DeviceStateTopic(site, "dev_back", contract.CapContact), contract.TypeDeviceState, device("dev_back", contract.CapContact, "open", ""), true)
	l.startHub()
	t.Cleanup(func() { l.stopHub() })
	l.waitState("disarmed")

	l.event("dev_keypad", contract.CapKeypad, "arm", "")
	l.waitCount(contract.TypeAlarmAck, 1)
	ack, _ := contract.DecodeData[contract.AlarmAckData](l.seen(contract.TypeAlarmAck)[0])
	if ack.Status != contract.AckRefused || ack.Reason != contract.ReasonZoneOpen {
		t.Fatalf("%+v: the hub must know the back door was left open\n%s", ack, l.logs.String())
	}
}

func TestTheHubWaitsForTheBrokerAndCatchesUp(t *testing.T) {
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state, "")
	broker.Stop() // the broker is not up when the hub starts

	l := &live{t: t, broker: broker, cfgPath: cfg, state: state, logs: &syncBuffer{}, ids: contract.NewIDGenerator(), got: make(chan struct{}, 1024)}
	l.startHub()
	t.Cleanup(func() { l.stopHub() })
	time.Sleep(1500 * time.Millisecond)
	select {
	case code := <-l.done:
		t.Fatalf("the hub gave up with exit code %d: it must wait for the broker\n%s", code, l.logs.String())
	default:
	}

	broker.Restart()
	l.client = dialTestClient(t, broker.URL(), l)
	l.waitState("disarmed") // the boot messages were kept and now go out
	l.waitCount(contract.TypeHealth, 1)
}

func TestTheHubRefusesToStartOnADamagedState(t *testing.T) {
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state, "")
	os.WriteFile(filepath.Join(state, "engine.json"), []byte(`{"mode": [`), 0o600)
	var out, errb syncBuffer
	code := run(context.Background(), []string{"run", "-config", cfg}, strings.NewReader(""), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "cannot start the hub") {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
}

func TestASecondHubOnTheSameStateIsRefused(t *testing.T) {
	state := t.TempDir()
	cfg := writeConfig(t, "mqtt://127.0.0.1:1", state, "")
	// Another live process (the test's parent) holds the pid file.
	pid := []byte(strconv.Itoa(os.Getppid()) + "\n")
	if err := os.WriteFile(filepath.Join(state, pidName), pid, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb syncBuffer
	code := run(context.Background(), []string{"run", "-config", cfg}, strings.NewReader(""), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "already using") {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
	if got, _ := os.ReadFile(filepath.Join(state, pidName)); string(got) != string(pid) {
		t.Fatal("the refused hub must leave the pid file of the running one alone")
	}

	// A pid file left by a dead process does not block a start.
	if err := os.WriteFile(filepath.Join(state, pidName), []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := pidfile.Lock(state, pidName)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestAnAcknowledgementOverTheBrokerEndsTheEscalation(t *testing.T) {
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state, "")
	l := startLive(t, broker, state, cfg)
	l.waitState("disarmed")
	l.event("dev_keypad", contract.CapKeypad, "arm", "")
	l.waitState("arming")
	l.event("dev_keypad", contract.CapKeypad, "arm_now", "")
	l.waitState("armed")
	l.event("dev_back", contract.CapContact, "open", "")
	l.waitCount(contract.TypeAlertRequest, 1)

	// The cloud reports the message sent, the owner answers "1" by app.
	req := l.seen(contract.TypeAlertRequest)[0]
	alert, _ := contract.DecodeData[contract.AlertRequestData](req)
	l.publish(contract.AlertResultTopic(site), contract.TypeAlertResult, contract.AlertResultData{Ref: req.ID, Status: contract.ResultSent}, false)
	l.publish(contract.AlertAckTopic(site), contract.TypeAlertAck, contract.AlertAckData{Incident: alert.Incident, Contact: "u_owner", Via: "app"}, false)
	l.waitFor("the acknowledgement to be taken", func() bool { return strings.Contains(l.logs.String(), "outcome=acknowledged") })
	l.validateEverything()
}
