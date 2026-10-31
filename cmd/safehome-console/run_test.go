package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/mqtt/mqtttest"
	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/hubconfig"
	"safehome/internal/install"
)

const site = "tg-lom-0001"

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// setup writes the hub's configuration (from its example) and the console's, both on
// the same state folder, with a fake nmcli that logs its calls.
func setup(t *testing.T, broker string, port int) (consolePath, hubPath, nmcliLog string) {
	t.Helper()
	dir, state := t.TempDir(), t.TempDir()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "hub.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(data), "/var/lib/safehome", state, 1)
	body = strings.Replace(body, "mqtt://127.0.0.1:1883", broker, 1)
	hubPath = filepath.Join(dir, "hub.json")
	os.WriteFile(hubPath, []byte(body), 0o600)
	os.WriteFile(filepath.Join(dir, "console.secret"), []byte("k7p2m9x4q8wd\n"), 0o600)
	consolePath = filepath.Join(dir, "console.json")
	os.WriteFile(consolePath, []byte(fmt.Sprintf(`{"site": %q, "broker_url": %q, "state_dir": %q, "hub_config": %q, "port": %d,
		"wifi": {"passphrase_file": "console.secret", "address": "127.0.0.1"}}`, site, broker, state, hubPath, port)), 0o600)

	nmcliLog = filepath.Join(dir, "nmcli.log")
	fake := filepath.Join(dir, "nmcli")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"$1 $2\" >> "+nmcliLog+"\n"), 0o755)
	old, oldForwarding, oldScript := nmcli, forwarding, dispatcherScript
	nmcli, forwarding, dispatcherScript = fake, filepath.Join(dir, "conf"), filepath.Join(dir, "50-safehome-console")
	t.Cleanup(func() { nmcli, forwarding, dispatcherScript = old, oldForwarding, oldScript })
	// What the dispatcher script leaves once the network is up: nothing forwarded.
	os.MkdirAll(filepath.Join(forwarding, "wlan0"), 0o755)
	os.WriteFile(filepath.Join(forwarding, "wlan0", "forwarding"), []byte("0\n"), 0o644)
	os.WriteFile(dispatcherScript, []byte("#!/bin/sh\n"), 0o755)
	return consolePath, hubPath, nmcliLog
}

func cli(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestUsageVersionAndCheck(t *testing.T) {
	if code, out, _ := cli("version"); code != 0 || !strings.Contains(out, "safehome-console") {
		t.Fatalf("%d %q", code, out)
	}
	for _, args := range [][]string{{}, {"fly"}, {"run"}, {"check", "-nonsense"}} {
		if code, _, _ := cli(args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	path, _, _ := setup(t, "mqtt://127.0.0.1:1883", 80)
	code, out, errb := cli("check", "-config", path)
	if code != 0 || !strings.Contains(out, `network "SeQ-tg-lom-0001" on wlan0, page at http://127.0.0.1/`) {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	if strings.Contains(out+errb, "k7p2m9x4q8wd") {
		t.Fatal("the passphrase is printed")
	}
	if strings.Contains(out, "warning") {
		t.Fatalf("warnings on a good install: %q", out)
	}
	os.Remove(dispatcherScript)
	if code, out, _ := cli("check", "-config", path); code != 0 || !strings.Contains(out, "50-safehome-console is missing") {
		t.Fatalf("no dispatcher script: %d %q", code, out)
	}

	// The console writes in the hub's state folder: it must be the same as its own.
	data, _ := os.ReadFile(path)
	os.WriteFile(path, bytes.Replace(data, []byte(`"state_dir": "`), []byte(`"state_dir": "/elsewhere`), 1), 0o600)
	if code, _, errb := cli("check", "-config", path); code != 1 || !strings.Contains(errb, "must be the hub's") {
		t.Fatalf("%d %q", code, errb)
	}
	if code, _, errb := cli("check", "-config", filepath.Join(t.TempDir(), "none.json")); code != 1 || !strings.Contains(errb, "configuration error") {
		t.Fatalf("%d %q", code, errb)
	}
}

// hubSide plays the hub and the Zigbee adapter on the broker.
type hubSide struct {
	t      *testing.T
	client *mqtt.Client
	ids    *contract.IDGenerator
	mu     sync.Mutex
	got    []contract.Envelope
}

func newHubSide(t *testing.T, ctx context.Context, broker string) *hubSide {
	t.Helper()
	h := &hubSide{t: t, ids: contract.NewIDGenerator()}
	c, err := mqtt.Dial(ctx, mqtt.Options{
		Username: mqtttest.Peer, Password: mqtttest.Password(mqtttest.Peer),
		BrokerURL: broker, ClientID: "hub-side-" + t.Name(), Ephemeral: true,
		Subscriptions: []mqtt.Subscription{{Filter: contract.InstallCmdTopic(site), QoS: 1}},
		OnMessage: func(m mqtt.Message) {
			if env, err := contract.ParseEnvelope(m.Payload); err == nil {
				h.mu.Lock()
				h.got = append(h.got, env)
				h.mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Await(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	h.client = c
	return h
}

func (h *hubSide) publish(topic, typ string, data any, retain bool) {
	h.t.Helper()
	env, err := contract.NewEnvelope(site, "test", 1, h.ids.New(), time.Now(), typ, data)
	if err != nil {
		h.t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.client.Publish(ctx, mqtt.Message{Topic: topic, Payload: b, QoS: 1, Retain: retain}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *hubSide) closes() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, env := range h.got {
		if env.Type == contract.TypeInstallClose {
			n++
		}
	}
	return n
}

func TestTheConsoleEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end scenario of about 10 s: run without -short")
	}
	broker := mqtttest.Start(t, "safehome-console")
	port := freePort(t)
	consolePath, hubPath, nmcliLog := setup(t, broker.URL(), port)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hub := newHubSide(t, ctx, broker.URL())
	devices := []contract.RegistryDevice{
		{DeviceID: "dev_door", Model: "aqara.contact", Capabilities: []string{"contact", "battery"}, Adapter: contract.AdapterZigbee},
		{DeviceID: "dev_siren1", Model: "heiman.siren", Capabilities: []string{"siren"}, Adapter: contract.AdapterZigbee},
		{DeviceID: "dev_k7p2", Model: "aqara.motion", Capabilities: []string{"motion", "battery"}, Adapter: contract.AdapterZigbee},
	}
	hub.publish(contract.RegistryDevicesTopic(site), contract.TypeRegistrySnapshot, contract.RegistrySnapshotData{Devices: devices}, true)
	hub.publish(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus,
		contract.ZigbeeStatusData{Bridge: contract.BridgeOnline, Devices: 3, Offline: []string{}}, true)

	logs := drivertest.Logs(t)
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(runCtx, []string{"run", "-config", consolePath}, io.Discard, logs) }()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	page := func(path string) (int, string) {
		resp, err := http.Get(base + path)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	time.Sleep(300 * time.Millisecond)
	if code, _ := page("/"); code != 0 {
		t.Fatal("the page is served outside the installation mode")
	}

	// The hub opens the mode: the network starts, then the page.
	hub.publish(contract.InstallModeTopic(site), contract.TypeInstallMode,
		contract.InstallModeData{Status: contract.InstallOpen, Until: contract.TSPtr(time.Now().Add(30 * time.Minute))}, true)
	drivertest.WaitFor(t, "the page", func() bool { code, body := page("/"); return code == 200 && strings.Contains(body, "dev_k7p2") })
	if log, _ := os.ReadFile(nmcliLog); !strings.Contains(string(log), "connection up") {
		t.Fatalf("nmcli calls: %s", log)
	}

	// A zone from the phone lands in the hub's state folder.
	resp, err := http.PostForm(base+"/zone", url.Values{"id": {"dev_k7p2"}, "name": {"Reserve"}, "kind": {"interior"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "zone enregistr") {
		t.Fatalf("after the zone: %s", body)
	}
	if s, err := hubconfig.ReadSite(hubPath); err != nil || s.Zones["dev_k7p2"] != (install.Zone{Name: "Reserve", Kind: "interior"}) {
		t.Fatalf("site %+v %v", s, err)
	}

	// Finish asks the hub to close; the hub says closed: the page and the network go.
	if resp, err := http.PostForm(base+"/finish", nil); err == nil {
		resp.Body.Close()
	}
	drivertest.WaitFor(t, "install.close on the broker", func() bool { return hub.closes() == 1 })
	hub.publish(contract.InstallModeTopic(site), contract.TypeInstallMode,
		contract.InstallModeData{Status: contract.InstallClosed, Reason: contract.InstallStopped}, true)
	drivertest.WaitFor(t, "the page gone", func() bool { code, _ := page("/"); return code == 0 })
	drivertest.WaitFor(t, "the network gone", func() bool {
		log, _ := os.ReadFile(nmcliLog)
		return strings.HasSuffix(strings.TrimSpace(string(log)), "connection delete")
	})

	stop()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the console did not stop")
	}
	if strings.Contains(logs.String(), "k7p2m9x4q8wd") {
		t.Fatal("the passphrase shows in the logs")
	}
}
