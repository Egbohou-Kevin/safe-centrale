package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/mqtt/mqtttest"
	"safehome/internal/adapter/pidfile"
	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/schematest"
)

const site = "tg-lom-0001"

func writeConfig(t *testing.T, broker, state string) string {
	t.Helper()
	profiles, err := filepath.Abs(filepath.Join("..", "..", "config", "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{
  "site": %q, "broker_url": %q, "client_id": "zigbee-test-%d", "state_dir": %q, "log_level": "debug",
  "profiles_dir": %q
}`, site, broker, time.Now().UnixNano(), state, profiles)
	path := filepath.Join(t.TempDir(), "zigbee.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func cli(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestUsageVersionAndCheck(t *testing.T) {
	if code, out, _ := cli("version"); code != 0 || !strings.Contains(out, "safehome-zigbee") {
		t.Fatalf("%d %q", code, out)
	}
	for _, args := range [][]string{{}, {"fly"}, {"run"}, {"check", "-nonsense"}} {
		if code, _, errb := cli(args...); code != 2 || errb == "" {
			t.Errorf("%v: %d %q", args, code, errb)
		}
	}
	code, out, _ := cli("check", "-config", writeConfig(t, "mqtt://127.0.0.1:1", t.TempDir()))
	if code != 0 || !strings.Contains(out, "5 profile(s)") || !strings.Contains(out, "heiman.siren.hs2wd") {
		t.Fatalf("%d %q", code, out)
	}
	if code, _, errb := cli("check", "-config", filepath.Join(t.TempDir(), "none.json")); code != 1 || !strings.Contains(errb, "configuration error") {
		t.Fatalf("%d %q", code, errb)
	}
}

func TestTheConfigExampleChecks(t *testing.T) {
	if code, out, errb := cli("check", "-config", filepath.Join("..", "..", "config", "zigbee.example.json")); code != 0 {
		t.Fatalf("%d %q %q", code, out, errb)
	}
}

func TestDeviceIDs(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		id := newDeviceID()
		if len(id) != 8 || !strings.HasPrefix(id, "dev_") || strings.Trim(id[4:], idAlphabet) != "" {
			t.Fatalf("%q", id)
		}
		seen[id] = true
	}
	if len(seen) < 190 {
		t.Fatalf("only %d different ids out of 200", len(seen))
	}
}

func TestASecondAdapterOnTheSameStateIsRefused(t *testing.T) {
	state := t.TempDir()
	cfg := writeConfig(t, "mqtt://127.0.0.1:1", state)
	running, err := pidfile.Lock(state, pidName) // a copy that runs holds the pid file
	if err != nil {
		t.Fatal(err)
	}
	defer running()
	if code, _, errb := cli("run", "-config", cfg); code != 1 || !strings.Contains(errb, "already using") {
		t.Fatalf("%d %q", code, errb)
	}
}

// fake is the test's MQTT client: it plays Zigbee2MQTT, the engine and the
// installer, and records everything.
type fake struct {
	mu     sync.Mutex
	msgs   []mqtt.Message
	client *mqtt.Client
	ids    *contract.IDGenerator
}

func (f *fake) on(topic string) []mqtt.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mqtt.Message
	for _, m := range f.msgs {
		if m.Topic == topic {
			out = append(out, m)
		}
	}
	return out
}

func (f *fake) of(typ string) []contract.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []contract.Envelope
	for _, m := range f.msgs {
		if env, err := contract.ParseEnvelope(m.Payload); err == nil && env.Type == typ && env.Src == contract.AdapterZigbee {
			out = append(out, env)
		}
	}
	return out
}

func (f *fake) publish(t *testing.T, topic, payload string, retain bool) {
	t.Helper()
	if err := f.client.Publish(context.Background(), mqtt.Message{Topic: topic, Payload: []byte(payload), QoS: 1, Retain: retain}); err != nil {
		t.Fatal(err)
	}
}

func (f *fake) order(t *testing.T, topic, typ string, data any) string {
	t.Helper()
	id := f.ids.New()
	env, err := contract.NewEnvelope(site, "test", 1, id, time.Now(), typ, data)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	f.publish(t, topic, string(b), false)
	return id
}

func TestTheAdapterEndToEndWithASimulatedZigbee2MQTT(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end scenario of about 10 s: run without -short")
	}
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state)

	f := &fake{ids: contract.NewIDGenerator()}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mqtt.Dial(ctx, mqtt.Options{
		BrokerURL: broker.URL(), ClientID: fmt.Sprintf("fake-%d", time.Now().UnixNano()),
		Subscriptions: []mqtt.Subscription{{Filter: "safehome/" + site + "/#", QoS: 1}, {Filter: "zigbee2mqtt/#", QoS: 1}},
		OnMessage:     func(m mqtt.Message) { f.mu.Lock(); f.msgs = append(f.msgs, m); f.mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Await(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	f.client = client
	f.publish(t, "zigbee2mqtt/bridge/state", `{"state":"online"}`, true) // retained, as Zigbee2MQTT does

	logs := drivertest.Logs(t)
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(runCtx, []string{"run", "-config", cfg}, &drivertest.SyncBuffer{}, logs) }()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}()

	status := func() contract.ZigbeeStatusData {
		st := f.of(contract.TypeZigbeeStatus)
		if len(st) == 0 {
			return contract.ZigbeeStatusData{}
		}
		d, _ := contract.DecodeData[contract.ZigbeeStatusData](st[len(st)-1])
		return d
	}
	drivertest.WaitFor(t, "the bridge online", func() bool { return status().Bridge == contract.BridgeOnline })

	// The installer pairs a door sensor and a siren.
	pair := func(ieee, model string) string {
		t.Helper()
		before := len(f.of(contract.TypeRegistryDeviceAdded))
		asked := len(f.on("zigbee2mqtt/bridge/request/permit_join"))
		f.order(t, contract.RegistryCmdTopic(site), contract.TypeRegistryPair, contract.RegistryPairData{DurationS: 120})
		// Zigbee2MQTT 2.x opens the network and echoes the request's transaction.
		drivertest.WaitFor(t, "the request to open", func() bool { return len(f.on("zigbee2mqtt/bridge/request/permit_join")) > asked })
		var req map[string]any
		json.Unmarshal(f.on("zigbee2mqtt/bridge/request/permit_join")[asked].Payload, &req)
		answer, _ := json.Marshal(map[string]any{"data": map[string]any{"time": req["time"]}, "status": "ok", "transaction": req["transaction"]})
		f.publish(t, "zigbee2mqtt/bridge/response/permit_join", string(answer), false)
		drivertest.WaitFor(t, "the network open", func() bool { return status().Pairing })
		f.publish(t, "zigbee2mqtt/bridge/event", `{"type":"device_interview","data":{"ieee_address":"`+ieee+`","status":"successful","supported":true,"definition":{"model":"`+model+`"}}}`, false)
		drivertest.WaitFor(t, model+" added", func() bool { return len(f.of(contract.TypeRegistryDeviceAdded)) == before+1 })
		d, _ := contract.DecodeData[contract.RegistryDeviceAddedData](f.of(contract.TypeRegistryDeviceAdded)[before])
		return d.DeviceID
	}
	door := pair("0x00158d0001000001", "MCCGQ11LM")
	siren := pair("0x00158d0001000002", "HS2WD-E")
	if len(f.on("zigbee2mqtt/bridge/request/permit_join")) == 0 || len(f.on("zigbee2mqtt/bridge/request/device/rename")) != 2 {
		t.Fatal("Zigbee2MQTT was asked to open the network and to rename the devices")
	}

	// Zigbee2MQTT, now using the new name, reports the door: closed, then open.
	f.publish(t, "zigbee2mqtt/"+door, `{"contact":true,"battery":95,"linkquality":120}`, false)
	f.publish(t, "zigbee2mqtt/"+door, `{"contact":false,"battery":95,"linkquality":120}`, false)
	contactEvent := func() bool {
		for _, m := range f.on(contract.DeviceEventTopic(site, door)) {
			env, _ := contract.ParseEnvelope(m.Payload)
			d, _ := contract.DecodeData[contract.DeviceEventData](env)
			if d.Capability == contract.CapContact && d.StringValue() == contract.ValOpen {
				return true
			}
		}
		return false
	}
	drivertest.WaitFor(t, "the door event", contactEvent)
	drivertest.WaitFor(t, "the door's state, retained on its own topic", func() bool {
		return len(f.on(contract.DeviceStateTopic(site, door, contract.CapContact))) > 0
	})

	// The engine sounds the siren.
	ref := f.order(t, contract.DeviceCmdTopic(site, siren), contract.TypeCommand, contract.DeviceCommandData{
		DeviceID: siren, Command: contract.CmdSirenOn, Args: contract.DeviceCommandArgs{DurationS: 300},
		Exp: contract.TSPtr(time.Now().Add(30 * time.Second)),
	})
	drivertest.WaitFor(t, "the siren request", func() bool { return len(f.on("zigbee2mqtt/0x00158d0001000002/set")) == 1 })
	var body map[string]map[string]any
	json.Unmarshal(f.on("zigbee2mqtt/0x00158d0001000002/set")[0].Payload, &body)
	if body["warning"]["mode"] != "burglar" || body["warning"]["duration"] != 300.0 {
		t.Fatalf("%v", body)
	}
	drivertest.WaitFor(t, "the acknowledgements", func() bool { return len(f.on(contract.DeviceAckTopic(site, siren))) == 2 })
	for _, m := range f.on(contract.DeviceAckTopic(site, siren)) {
		env, _ := contract.ParseEnvelope(m.Payload)
		a, _ := contract.DecodeData[contract.CommandAckData](env)
		if a.Ref != ref || (a.Status != contract.CmdAccepted && a.Status != contract.CmdDone) {
			t.Fatalf("%+v", a)
		}
	}

	// Everything published follows the contract.
	files := map[string]string{
		contract.TypeDeviceEvent: "device-event.json", contract.TypeDeviceState: "device-event.json",
		contract.TypeCommandAck: "device-command-ack.json", contract.TypeRegistrySnapshot: "registry-snapshot.json",
		contract.TypeRegistryDeviceAdded: "registry-device-added.json", contract.TypeZigbeeStatus: "zigbee-status.json",
	}
	f.mu.Lock()
	msgs := append([]mqtt.Message(nil), f.msgs...)
	f.mu.Unlock()
	for _, m := range msgs {
		e, err := contract.ParseEnvelope(m.Payload)
		if err != nil || e.Src != contract.AdapterZigbee {
			continue
		}
		file, ok := files[e.Type]
		if !ok {
			t.Errorf("unexpected %s", e.Type)
			continue
		}
		if err := schematest.Check(file, e.Data); err != nil {
			t.Errorf("%s: %v", e.Type, err)
		}
	}

	// A clean stop removes the pid file.
	stop()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the adapter did not stop")
	}
	if _, err := os.Stat(filepath.Join(state, pidName)); !os.IsNotExist(err) {
		t.Fatal("the pid file must be removed")
	}
}
