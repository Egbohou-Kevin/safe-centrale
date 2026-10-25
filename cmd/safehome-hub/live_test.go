package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/mqtt/mqtttest"
	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/schematest"
)

// live is a running hub and a test client on the same broker.
type live struct {
	t       *testing.T
	broker  *mqtttest.Broker
	cfgPath string
	state   string
	logs    *drivertest.SyncBuffer
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
	l := &live{t: t, broker: broker, cfgPath: cfgPath, state: state, logs: drivertest.Logs(t), ids: contract.NewIDGenerator(), got: make(chan struct{}, 1024)}
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
		l.done <- run(ctx, []string{"run", "-config", l.cfgPath}, strings.NewReader(""), &drivertest.SyncBuffer{}, l.logs)
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
	drivertest.WaitFor(l.t, "state "+want, func() bool { return l.lastState() == want })
}

func (l *live) waitCount(typ string, n int) {
	l.t.Helper()
	drivertest.WaitFor(l.t, fmt.Sprintf("%d x %s", n, typ), func() bool { return len(l.seen(typ)) >= n })
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
	contract.TypeDevices:     "system-devices.json",
	contract.TypeInstallMode: "install-mode.json", contract.TypeAlarmSirenFailed: "alarm-siren-failed.json",
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
	drivertest.WaitFor(l.t, fmt.Sprintf("%d config.applied", n), func() bool { return len(l.applied()) >= n })
	return l.applied()[n-1]
}

func lastSeq(l *live) uint64 {
	var last uint64
	for _, m := range l.allFromHub() {
		env, _ := contract.ParseEnvelope(m.Payload)
		last = max(last, env.Seq)
	}
	return last
}

// fromHub tells the hub's own messages from the test client's.
func fromHub(env contract.Envelope) bool {
	return env.Src == "engine" || env.Src == "health" || env.Src == "escalation" || env.Src == "install"
}
