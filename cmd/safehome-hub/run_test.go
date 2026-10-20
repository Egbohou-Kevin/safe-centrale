package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/mqtt/mqtttest"
	"safehome/internal/adapter/pidfile"
	"safehome/internal/contract"
	"safehome/internal/hubconfig"
	"safehome/internal/schematest"
	"safehome/internal/siteconfig"
)

const site = "tg-lom-0001"

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// writeConfig writes a configuration for a test hub and returns its path.
func writeConfig(t *testing.T, brokerURL, stateDir string, extra string) string {
	t.Helper()
	body := fmt.Sprintf(`{
  "site": %q,
  "broker_url": %q,
  "client_id": "safehome-hub-test-%d",
  "state_dir": %q,
  "log_level": "debug",
  "hub_device_id": "dev_hubpower",
  "siren_ids": ["dev_siren1"],
  "contacts": [{"id": "u_owner", "name": "Owner", "role": "owner", "channels": ["app", "sms"]}],
  "zones": {
    "dev_door": {"name": "Entrance", "kind": "entry"},
    "dev_back": {"name": "Back", "kind": "instant"}
  },
  "access": {"iterations": 10000},
  "heartbeat_every": "1h",
  "boot_window": "300ms"%s
}`, site, brokerURL, time.Now().UnixNano(), stateDir, extra)
	path := filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// cli runs the program's command line and returns its exit code and outputs.
func cli(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

// --- the command line --------------------------------------------------------------------

func TestUsageAndVersion(t *testing.T) {
	if code, out, _ := cli(t, "", "version"); code != 0 || !strings.Contains(out, "safehome-hub") {
		t.Fatalf("%d %q", code, out)
	}
	for _, args := range [][]string{{}, {"fly"}, {"run"}, {"check"}, {"codes"}, {"run", "-nonsense"}} {
		if code, _, errb := cli(t, "", args...); code != 2 || errb == "" {
			t.Errorf("%v: exit %d, stderr %q", args, code, errb)
		}
	}
}

func TestCheck(t *testing.T) {
	path := writeConfig(t, "mqtt://127.0.0.1:1883", t.TempDir(), "")
	code, out, errb := cli(t, "", "check", "-config", path)
	if code != 0 || !strings.Contains(out, "configuration OK") || !strings.Contains(out, "2 zone(s)") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"site": "tg-lom-0001", "exit_dealy": "2m"}`), 0o600)
	if code, _, errb := cli(t, "", "check", "-config", bad); code != 1 || !strings.Contains(errb, "configuration error") {
		t.Fatalf("%d %q", code, errb)
	}
	if code, _, _ := cli(t, "", "check", "-config", filepath.Join(t.TempDir(), "missing.json")); code != 1 {
		t.Fatalf("a missing file: exit %d", code)
	}
}

func TestCodesCommands(t *testing.T) {
	state := t.TempDir()
	path := writeConfig(t, "mqtt://127.0.0.1:1883", state, "")

	if code, out, _ := cli(t, "", "codes", "-config", path, "list"); code != 0 || !strings.Contains(out, "no access code") {
		t.Fatalf("%d %q", code, out)
	}
	code, out, errb := cli(t, "482915\n", "codes", "-config", path, "set", "u_owner")
	if code != 0 || !strings.Contains(out, "code set for u_owner") || !strings.Contains(out, "no hub is running") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	if strings.Contains(out+errb, "482915") {
		t.Fatal("the code must never be printed")
	}
	cli(t, "730264\n", "codes", "-config", path, "set", "u_guard")
	if _, out, _ := cli(t, "", "codes", "-config", path, "list"); out != "u_guard\nu_owner\n" {
		t.Fatalf("%q", out)
	}

	// What is refused, and why, without ever echoing the code.
	for name, c := range map[string]struct {
		stdin string
		args  []string
		want  string
	}{
		"weak code":    {"1234\n", []string{"set", "u_x"}, "too easy to guess"},
		"not digits":   {"48a915\n", []string{"set", "u_x"}, "4 to 8 digits"},
		"taken":        {"482915\n", []string{"set", "u_x"}, "already has this code"},
		"no code":      {"", []string{"set", "u_x"}, "no code given"},
		"blank code":   {"   \n", []string{"set", "u_x"}, "no code given"},
		"unknown user": {"", []string{"remove", "nobody"}, "no such user"},
	} {
		code, out, errb := cli(t, c.stdin, append([]string{"codes", "-config", path}, c.args...)...)
		if code != 1 || !strings.Contains(errb, c.want) {
			t.Errorf("%s: exit %d stderr %q", name, code, errb)
		}
		if strings.Contains(out+errb, "48a915") || strings.Contains(out+errb, "482915") {
			t.Errorf("%s: the code is echoed back: %q %q", name, out, errb)
		}
	}
	if code, _, _ := cli(t, "", "codes", "-config", path, "set"); code != 2 {
		t.Errorf("set without a user: exit %d", code)
	}
	if code, _, _ := cli(t, "", "codes", "-config", path, "remove"); code != 2 {
		t.Errorf("remove without a user: exit %d", code)
	}
	if code, _, _ := cli(t, "", "codes", "-config", path, "explode"); code != 2 {
		t.Errorf("unknown subcommand: exit %d", code)
	}

	if code, out, _ := cli(t, "", "codes", "-config", path, "remove", "u_guard"); code != 0 || !strings.Contains(out, "code removed for u_guard") {
		t.Fatalf("%d %q", code, out)
	}
	if _, out, _ := cli(t, "", "codes", "-config", path, "list"); out != "u_owner\n" {
		t.Fatalf("%q", out)
	}

	// The installer code: apart from the people's, and never the same as one of them.
	if code, _, errb := cli(t, "482915\n", "codes", "-config", path, "set", "installer"); code != 1 || !strings.Contains(errb, "already has this code") {
		t.Fatalf("an installer code equal to the owner's: %d %q", code, errb)
	}
	if code, out, errb := cli(t, "259031\n", "codes", "-config", path, "set", "installer"); code != 0 || !strings.Contains(out, "code set for installer") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	if code, _, errb := cli(t, "259031\n", "codes", "-config", path, "set", "u_guard"); code != 1 || !strings.Contains(errb, "already has this code") {
		t.Fatalf("a person's code equal to the installer's: %d %q", code, errb)
	}
	if _, out, _ := cli(t, "", "codes", "-config", path, "list"); out != "u_owner\ninstaller\n" {
		t.Fatalf("%q", out)
	}

	// The codes are saved hashed: nothing in the state directory holds a code.
	entries, _ := os.ReadDir(state)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(state, e.Name()))
		if strings.Contains(string(b), "482915") {
			t.Errorf("%s holds a code in clear", e.Name())
		}
	}
}

// --- the whole program against a real broker ---------------------------------------------

// live is a running hub and a test client on the same broker.
type live struct {
	t       *testing.T
	broker  *mqtttest.Broker
	cfgPath string
	state   string
	logs    *syncBuffer
	cancel  context.CancelFunc
	done    chan int
	client  *mqtt.Client
	ids     *contract.IDGenerator
	seq     uint64

	mu   sync.Mutex
	msgs []mqtt.Message
	got  chan struct{}
}

func startLive(t *testing.T, broker *mqtttest.Broker, state, cfgPath string) *live {
	t.Helper()
	l := &live{t: t, broker: broker, cfgPath: cfgPath, state: state, logs: &syncBuffer{}, ids: contract.NewIDGenerator(), got: make(chan struct{}, 1024)}
	l.client = dialTestClient(t, broker.URL(), l)
	l.startHub()
	t.Cleanup(func() { l.stopHub() })
	return l
}

func dialTestClient(t *testing.T, url string, l *live) *mqtt.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, err := mqtt.Dial(ctx, mqtt.Options{
		BrokerURL: url, ClientID: fmt.Sprintf("test-client-%d", time.Now().UnixNano()),
		Subscriptions: []mqtt.Subscription{{Filter: "safehome/" + site + "/#", QoS: 1}},
		OnMessage: func(m mqtt.Message) {
			l.mu.Lock()
			l.msgs = append(l.msgs, m)
			l.mu.Unlock()
			l.got <- struct{}{}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	if err := c.Await(waitCtx); err != nil {
		t.Fatal(err)
	}
	return c
}

func (l *live) startHub() {
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.done = make(chan int, 1)
	go func() {
		l.done <- run(ctx, []string{"run", "-config", l.cfgPath}, strings.NewReader(""), &syncBuffer{}, l.logs)
	}()
}

func (l *live) stopHub() int {
	if l.cancel == nil {
		return -1
	}
	l.cancel()
	l.cancel = nil
	select {
	case code := <-l.done:
		return code
	case <-time.After(15 * time.Second):
		l.t.Fatalf("the hub did not stop\nlogs:\n%s", l.logs.String())
		return -1
	}
}

// publish sends an envelope on a topic, as a device or the cloud would.
func (l *live) publish(topic, typ string, data any, retain bool) {
	l.t.Helper()
	l.seq++
	env, err := contract.NewEnvelope(site, "test", l.seq, l.ids.New(), time.Now(), typ, data)
	if err != nil {
		l.t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.client.Publish(ctx, mqtt.Message{Topic: topic, Payload: b, QoS: 1, Retain: retain}); err != nil {
		l.t.Fatal(err)
	}
}

func device(id, capability, value, code string) contract.DeviceEventData {
	d := contract.DeviceEventData{DeviceID: id, Capability: capability, Value: json.RawMessage(`"` + value + `"`), Code: code}
	if capability == contract.CapBattery {
		d.Value = json.RawMessage(value)
	}
	return d
}

func (l *live) event(id, capability, value, code string) {
	l.t.Helper()
	l.publish(contract.DeviceEventTopic(site, id), contract.TypeDeviceEvent, device(id, capability, value, code), false)
}

// seen returns the messages of a type received so far, from the hub only.
func (l *live) seen(typ string) []contract.Envelope {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []contract.Envelope
	for _, m := range l.msgs {
		env, err := contract.ParseEnvelope(m.Payload)
		if err == nil && env.Type == typ && fromHub(env) {
			out = append(out, env)
		}
	}
	return out
}

func (l *live) waitFor(what string, cond func() bool) {
	l.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			l.t.Fatalf("timed out waiting for %s\nhub logs:\n%s", what, l.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (l *live) lastState() string {
	states := l.seen(contract.TypeAlarmState)
	if len(states) == 0 {
		return ""
	}
	d, _ := contract.DecodeData[contract.AlarmStateData](states[len(states)-1])
	return d.State
}

func (l *live) waitState(want string) {
	l.t.Helper()
	l.waitFor("state "+want, func() bool { return l.lastState() == want })
}

func (l *live) waitCount(typ string, n int) {
	l.t.Helper()
	l.waitFor(fmt.Sprintf("%d x %s", n, typ), func() bool { return len(l.seen(typ)) >= n })
}

func (l *live) siren() []contract.DeviceCommandData {
	var out []contract.DeviceCommandData
	for _, env := range l.seen(contract.TypeCommand) {
		d, _ := contract.DecodeData[contract.DeviceCommandData](env)
		out = append(out, d)
	}
	return out
}

// allFromHub is every message the hub published, with its topic.
func (l *live) allFromHub() []mqtt.Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []mqtt.Message
	for _, m := range l.msgs {
		if env, err := contract.ParseEnvelope(m.Payload); err == nil && fromHub(env) {
			out = append(out, m)
		}
	}
	return out
}

var schemaFor = map[string]string{
	contract.TypeAlarmArming: "alarm-arming.json", contract.TypeAlarmArmed: "alarm-armed.json",
	contract.TypeAlarmDisarmed: "alarm-disarmed.json", contract.TypeAlarmEntryDelay: "alarm-entry-delay.json",
	contract.TypeAlarmTriggered: "alarm-triggered.json", contract.TypeAlarmSirenCycle: "alarm-siren-cycle.json",
	contract.TypeAlarmCleared: "alarm-cleared.json", contract.TypeAlarmAuthFailed: "alarm-auth-failed.json",
	contract.TypeAlarmAck: "alarm-ack.json", contract.TypeAlarmState: "alarm-state.json",
	contract.TypeAlertRequest: "alert-request.json", contract.TypeCommand: "device-command.json",
	contract.TypeHeartbeat: "system-heartbeat.json", contract.TypeHealth: "system-health.json",
	contract.TypeInstallMode: "install-mode.json",
}

// validateEverything checks that nothing the hub published breaks the contract, and
// that no secret travelled in it.
func (l *live) validateEverything(secrets ...string) {
	l.t.Helper()
	for _, m := range l.allFromHub() {
		env, _ := contract.ParseEnvelope(m.Payload)
		file, ok := schemaFor[env.Type]
		if !ok {
			l.t.Errorf("no schema for %s", env.Type)
			continue
		}
		if err := schematest.Check(file, env.Data); err != nil {
			l.t.Errorf("%s on %s: %v", env.Type, m.Topic, err)
		}
		if err := schematest.Check("envelope.json", m.Payload); err != nil {
			l.t.Errorf("envelope of %s: %v", env.Type, err)
		}
		for _, secret := range secrets {
			if strings.Contains(string(m.Payload), secret) {
				l.t.Errorf("a message of type %s carries a secret code", env.Type)
			}
		}
	}
	for _, secret := range secrets {
		if strings.Contains(l.logs.String(), secret) {
			l.t.Errorf("the hub's logs contain a secret code")
		}
	}
}

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
	if _, err := os.Stat(filepath.Join(state, "hub.pid")); !os.IsNotExist(err) {
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

func lastSeq(l *live) uint64 {
	var last uint64
	for _, m := range l.allFromHub() {
		env, _ := contract.ParseEnvelope(m.Payload)
		last = max(last, env.Seq)
	}
	return last
}

func TestCodesSetWhileTheHubRunsTakeEffectWithoutARestart(t *testing.T) {
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state, "")
	l := startLive(t, broker, state, cfg)
	l.waitState("disarmed")
	l.event("dev_keypad", contract.CapKeypad, "arm", "")
	l.waitState("arming")
	l.event("dev_keypad", contract.CapKeypad, "arm_now", "")
	l.waitState("armed")

	// The installer sets a code with the command-line tool; the running hub is told.
	code, out, errb := cli(t, "730264\n", "codes", "-config", cfg, "set", "u_guard")
	if code != 0 || !strings.Contains(out, "told to reload") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	l.waitFor("the reload", func() bool { return strings.Contains(l.logs.String(), "access codes reloaded") })
	l.event("dev_keypad", contract.CapKeypad, "disarm", "730264")
	l.waitState("disarmed")
	if d, _ := contract.DecodeData[contract.DisarmedData](l.seen(contract.TypeAlarmDisarmed)[0]); d.User != "u_guard" {
		t.Fatalf("%+v", d)
	}
	l.validateEverything("730264")
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
	if err := os.WriteFile(filepath.Join(state, "hub.pid"), pid, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb syncBuffer
	code := run(context.Background(), []string{"run", "-config", cfg}, strings.NewReader(""), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "already using") {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
	if got, _ := os.ReadFile(filepath.Join(state, "hub.pid")); string(got) != string(pid) {
		t.Fatal("the refused hub must leave the pid file of the running one alone")
	}

	// A pid file left by a dead process does not block a start.
	if err := os.WriteFile(filepath.Join(state, "hub.pid"), []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := pidfile.Lock(state, pidName)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// fromHub tells the hub's own messages from the test client's.
func fromHub(env contract.Envelope) bool {
	return env.Src == "engine" || env.Src == "health" || env.Src == "escalation" || env.Src == "install"
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

// --- the configuration set from the cloud ------------------------------------------------

// applied returns the config.applied answers received so far.
func (l *live) applied() []contract.ConfigAppliedData {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []contract.ConfigAppliedData
	for _, m := range l.msgs {
		env, err := contract.ParseEnvelope(m.Payload)
		if err != nil || env.Type != contract.TypeConfigApplied || env.Src != "config" {
			continue
		}
		if m.Topic != contract.ConfigAppliedTopic(site) {
			l.t.Errorf("config.applied on %s", m.Topic)
		}
		if err := schematest.Check("config-applied.json", env.Data); err != nil {
			l.t.Errorf("config.applied: %v", err)
		}
		d, _ := contract.DecodeData[contract.ConfigAppliedData](env)
		out = append(out, d)
	}
	return out
}

func (l *live) waitApplied(n int) contract.ConfigAppliedData {
	l.t.Helper()
	l.waitFor(fmt.Sprintf("%d config.applied", n), func() bool { return len(l.applied()) >= n })
	return l.applied()[n-1]
}

func TestAConfigurationFromTheCloudRestartsTheHubThenIsApplied(t *testing.T) {
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state, "")
	l := startLive(t, broker, state, cfg)
	l.waitState("disarmed")

	// Without an owner: refused at once, nothing kept, no restart.
	l.publish(contract.ConfigSetTopic(site), contract.TypeConfigSet, contract.ConfigSetData{Version: 2, Contacts: []contract.ConfigContact{
		{ID: "u_guard", Name: "Kofi", Role: "guard", Channels: []string{"sms"}, Phone: "+22870000002"},
	}}, false)
	if d := l.waitApplied(1); d.Version != 2 || d.Status != contract.ConfigRejected || !strings.Contains(d.Reason, "owner") {
		t.Fatalf("%+v", d)
	}
	if _, ok, _ := siteconfig.Read(state); ok {
		t.Fatal("a refused configuration was kept")
	}

	// A good one: kept, then the hub stops for systemd to start it again.
	l.publish(contract.ConfigSetTopic(site), contract.TypeConfigSet, contract.ConfigSetData{
		Version: 3, SiteName: "Boutique Be", EntryDelayS: 20,
		Contacts: []contract.ConfigContact{
			{ID: "u_owner", Name: "Ama", Role: "owner", Channels: []string{"app", "sms"}, Phone: "+22890000001"},
			{ID: "u_night", Name: "Kofi", Role: "guard", Channels: []string{"sms"}, Phone: "+22870000002"},
		},
	}, false)
	select {
	case code := <-l.done:
		if code != 0 {
			t.Fatalf("exit %d: systemd must see a clean stop", code)
		}
		l.cancel = nil
	case <-time.After(15 * time.Second):
		t.Fatalf("the hub did not restart\nlogs:\n%s", l.logs.String())
	}
	if d, ok, err := siteconfig.Read(state); err != nil || !ok || d.Version != 3 {
		t.Fatalf("%v %v %d", err, ok, d.Version)
	}

	// Started again, it runs with it and says so.
	l.startHub()
	if d := l.waitApplied(2); d.Version != 3 || d.Status != contract.ConfigApplied {
		t.Fatalf("%+v", d)
	}
	code, out, _ := cli(t, "", "check", "-config", cfg)
	if code != 0 || !strings.Contains(out, "2 contact(s)") || !strings.Contains(out, "from the cloud: version 3") {
		t.Fatalf("%d %q", code, out)
	}
	// The entry delay of 20 s from the cloud: an entry, then the alarm only after it.
	l.event("dev_keypad", contract.CapKeypad, "arm", "")
	l.waitState("arming")
	l.event("dev_keypad", contract.CapKeypad, "arm_now", "")
	l.waitState("armed")
	l.event("dev_door", contract.CapContact, "open", "")
	l.waitCount(contract.TypeAlarmEntryDelay, 1)
	env := l.seen(contract.TypeAlarmEntryDelay)[0]
	ed, _ := contract.DecodeData[contract.EntryDelayData](env)
	if got := ed.EndsAt.Sub(env.TS.Time).Round(time.Second); got != 20*time.Second {
		t.Fatalf("entry delay %v, the cloud said 20 s", got)
	}

	if strings.Contains(l.logs.String(), "0000001") || strings.Contains(l.logs.String(), "0000002") {
		t.Fatalf("a phone number in the logs:\n%s", l.logs.String())
	}
	l.validateEverything()
}

func TestTheInstallationModeAppliesNewZonesByRestarting(t *testing.T) {
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state, "")
	if code, _, errb := cli(t, "259031\n", "codes", "-config", cfg, "set", "installer"); code != 0 {
		t.Fatalf("%d %q", code, errb)
	}
	l := startLive(t, broker, state, cfg)
	l.waitState("disarmed")

	// The installer code at the keypad opens the mode; the console changes the zones,
	// then closes the mode: the hub stops for systemd to start it again.
	l.event("dev_keypad", contract.CapKeypad, "disarm", "259031")
	l.waitFor("the installation mode", func() bool {
		for _, env := range l.seen(contract.TypeInstallMode) {
			if d, _ := contract.DecodeData[contract.InstallModeData](env); d.Status == contract.InstallOpen {
				return true
			}
		}
		return false
	})
	devices, err := hubconfig.ReadSite(cfg)
	if err != nil {
		t.Fatal(err)
	}
	devices.Zones["dev_k7p2"] = hubconfig.Zone{Name: "Reserve", Kind: "instant"}
	if err := hubconfig.WriteSite(cfg, devices); err != nil {
		t.Fatal(err)
	}
	l.publish(contract.InstallCmdTopic(site), contract.TypeInstallClose, contract.InstallCloseData{}, false)
	select {
	case code := <-l.done:
		if code != 0 {
			t.Fatalf("exit %d: systemd must see a clean stop", code)
		}
		l.cancel = nil
	case <-time.After(15 * time.Second):
		t.Fatalf("the hub did not restart\nlogs:\n%s", l.logs.String())
	}

	// Started again, it watches the new zone.
	l.startHub()
	l.waitState("disarmed")
	l.event("dev_keypad", contract.CapKeypad, "arm", "")
	l.event("dev_keypad", contract.CapKeypad, "arm_now", "")
	l.waitState("armed")
	l.event("dev_k7p2", contract.CapContact, "open", "")
	l.waitState("triggered")
	if code, out, _ := cli(t, "", "check", "-config", cfg); code != 0 || !strings.Contains(out, "set by the installer's tools") {
		t.Fatalf("%d %q", code, out)
	}
	if strings.Contains(l.logs.String(), "259031") {
		t.Fatal("the installer code shows in the logs")
	}
	l.validateEverything()
}

// --- systemd ---------------------------------------------------------------------------

// fakeSystemd listens where systemd would, and collects what the program says.
func fakeSystemd(t *testing.T) func() []string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sdn") // a short path: Unix sockets have a small limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	t.Setenv("WATCHDOG_USEC", "300000") // 300 ms
	var mu sync.Mutex
	var got []string
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			mu.Lock()
			got = append(got, string(buf[:n]))
			mu.Unlock()
		}
	}()
	return func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }
}

func TestTheHubTellsSystemdItIsReadyAndStillWorks(t *testing.T) {
	said := fakeSystemd(t)
	broker := mqtttest.Start(t)
	state := t.TempDir()
	l := startLive(t, broker, state, writeConfig(t, broker.URL(), state, ""))
	l.waitState("disarmed")
	pings := func() int { return strings.Count(strings.Join(said(), "\n"), "WATCHDOG=1") }
	l.waitFor("ready, then pings", func() bool { return len(said()) > 0 && said()[0] == "READY=1" && pings() >= 3 })
	if code := l.stopHub(); code != 0 {
		t.Fatal(code)
	}
	if !slices.Contains(said(), "STOPPING=1") {
		t.Fatalf("%q", said())
	}
}
