package main

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

func init() {
	connectWait = 2 * time.Second
	snapshotWait = 500 * time.Millisecond
	timing = install.Timing{Confirm: time.Second, Open: time.Second, Grace: 200 * time.Millisecond}
}

// writeConfig copies the example configuration, pointed at the test broker.
func writeConfig(t *testing.T, broker string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "hub.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(data), "mqtt://127.0.0.1:1883", broker, 1)
	body = strings.Replace(body, "/var/lib/safehome", t.TempDir(), 1)
	path := filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func cli(ctx context.Context, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(ctx, args, &out, &errb)
	return code, out.String(), errb.String()
}

func readSite(t *testing.T, path string) install.Site {
	t.Helper()
	s, err := hubconfig.ReadSite(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeAdapter plays safehome-zigbee on the broker: a retained registry and status,
// answers to pairing and removal orders.
type fakeAdapter struct {
	t       *testing.T
	client  *mqtt.Client
	mu      sync.Mutex
	devices []contract.RegistryDevice
	join    []contract.RegistryDevice // devices that join at the next pairing
	refuse  string                    // Zigbee2MQTT's refusal of the next pairing, if any
	seq     uint64
}

func dev(id string, caps ...string) contract.RegistryDevice {
	return contract.RegistryDevice{DeviceID: id, Model: "model." + strings.TrimPrefix(id, "dev_"), Capabilities: caps, Adapter: contract.AdapterZigbee}
}

// exampleDevices are the devices the example configuration uses.
func exampleDevices() []contract.RegistryDevice {
	return []contract.RegistryDevice{
		dev("dev_door", "contact", "battery"), dev("dev_back", "contact", "battery"),
		dev("dev_window", "contact", "battery"), dev("dev_motion", "motion", "battery"),
		dev("dev_smoke", "smoke", "battery"), dev("dev_panic", "panic", "battery"),
		dev("dev_tamper", "tamper", "mains"), dev("dev_siren1", "siren", "mains"),
	}
}

func startAdapter(t *testing.T, ctx context.Context, broker string, devices []contract.RegistryDevice) *fakeAdapter {
	t.Helper()
	a := &fakeAdapter{t: t, devices: devices}
	c, err := mqtt.Dial(ctx, mqtt.Options{
		BrokerURL: broker, ClientID: "fake-adapter-" + t.Name(), Ephemeral: true,
		Subscriptions: []mqtt.Subscription{{Filter: contract.RegistryCmdTopic(site), QoS: 1}},
		OnMessage:     func(m mqtt.Message) { go a.order(ctx, m) },
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if err := c.Await(awaitCtx); err != nil {
		t.Fatal(err)
	}
	a.client = c
	a.publishRegistry(ctx)
	a.publish(ctx, contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(len(devices), false), true)
	return a
}

func status(n int, pairing bool) contract.ZigbeeStatusData {
	return contract.ZigbeeStatusData{Bridge: contract.BridgeOnline, Devices: n, Offline: []string{}, Pairing: pairing}
}

func (a *fakeAdapter) publish(ctx context.Context, topic, typ string, data any, retain bool) {
	a.mu.Lock()
	a.seq++
	seq := a.seq
	a.mu.Unlock()
	env, err := contract.NewEnvelope(site, contract.AdapterZigbee, seq, contract.NewIDGenerator().New(), time.Now(), typ, data)
	if err != nil {
		a.t.Error(err)
		return
	}
	b, _ := json.Marshal(env)
	if err := a.client.Publish(ctx, mqtt.Message{Topic: topic, Payload: b, QoS: 1, Retain: retain}); err != nil {
		a.t.Error(err)
	}
}

func (a *fakeAdapter) publishRegistry(ctx context.Context) {
	a.mu.Lock()
	devices := slices.Clone(a.devices)
	a.mu.Unlock()
	a.publish(ctx, contract.RegistryDevicesTopic(site), contract.TypeRegistrySnapshot, contract.RegistrySnapshotData{Devices: devices}, true)
}

func (a *fakeAdapter) order(ctx context.Context, m mqtt.Message) {
	env, err := contract.ParseEnvelope(m.Payload)
	if err != nil {
		a.t.Errorf("the tool sent an invalid order: %v", err)
		return
	}
	if env.Src != install.Source {
		a.t.Errorf("src %q", env.Src)
	}
	ev := contract.RegistryEventTopic(site)
	switch env.Type {
	case contract.TypeRegistryPair:
		d, _ := contract.DecodeData[contract.RegistryPairData](env)
		if d.DurationS == 0 {
			return
		}
		a.mu.Lock()
		join, refuse := a.join, a.refuse
		a.join, a.refuse = nil, ""
		a.devices = append(a.devices, join...)
		n := len(a.devices)
		a.mu.Unlock()
		if refuse != "" {
			// As the adapter: never said open, since Zigbee2MQTT did not confirm.
			a.publish(ctx, ev, contract.TypeRegistryPairFailed, contract.RegistryPairFailedData{Ref: env.ID, Reason: refuse}, false)
			return
		}
		a.publish(ctx, contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(n, true), true)
		for _, j := range join {
			a.publish(ctx, ev, contract.TypeRegistryDeviceAdded, contract.RegistryDeviceAddedData{Ref: env.ID, DeviceID: j.DeviceID, Model: j.Model, Capabilities: j.Capabilities}, false)
		}
		a.publish(ctx, ev, contract.TypeRegistryDeviceRejected, contract.RegistryDeviceRejectedData{Ref: env.ID, Model: "TS0203", Reason: contract.RejectUnknownModel}, false)
		a.publishRegistry(ctx)
		a.publish(ctx, contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(n, false), true)
	case contract.TypeRegistryRemove:
		d, _ := contract.DecodeData[contract.RegistryRemoveData](env)
		a.mu.Lock()
		a.devices = slices.DeleteFunc(a.devices, func(x contract.RegistryDevice) bool { return x.DeviceID == d.DeviceID })
		a.mu.Unlock()
		a.publish(ctx, ev, contract.TypeRegistryDeviceRemoved, contract.RegistryDeviceRemovedData{Ref: env.ID, DeviceID: d.DeviceID}, false)
		a.publishRegistry(ctx)
	}
}

func TestUsageAndArguments(t *testing.T) {
	ctx := context.Background()
	if code, out, _ := cli(ctx, "version"); code != 0 || !strings.Contains(out, "safehome-install") {
		t.Fatalf("%d %q", code, out)
	}
	path := writeConfig(t, "mqtt://127.0.0.1:1")
	for _, args := range [][]string{
		{}, {"fly"}, {"devices"}, {"devices", "-nonsense"}, {"remove", "-config", path},
		{"zone", "-config", path, "a", "b"}, {"siren", "-config", path, "add"},
		{"siren", "-config", path, "paint", "dev_door"}, {"siren", "-config", path, "name"},
	} {
		if code, _, errb := cli(ctx, args...); code != 2 || errb == "" {
			t.Errorf("%v: %d %q", args, code, errb)
		}
	}
	if code, _, errb := cli(ctx, "check", "-config", filepath.Join(t.TempDir(), "none.json")); code != 1 || !strings.Contains(errb, "configuration error") {
		t.Fatalf("%d %q", code, errb)
	}
}

func TestOfflineChangesNeedNoBroker(t *testing.T) {
	ctx := context.Background()
	path := writeConfig(t, "mqtt://127.0.0.1:1")

	code, out, errb := cli(ctx, "unzone", "-config", path, "dev_window")
	if code != 0 || !strings.Contains(out, `zone "Vitrine" removed`) || !strings.Contains(out, "systemctl restart safehome-hub") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	if _, ok := readSite(t, path).Zones["dev_window"]; ok {
		t.Fatal("the zone is still in the file")
	}
	if code, _, errb := cli(ctx, "unzone", "-config", path, "dev_window"); code != 1 || !strings.Contains(errb, "has no zone") {
		t.Fatalf("%d %q", code, errb)
	}

	// Flags after the device work as well.
	// The hub refuses a site without siren: the last one cannot go.
	if code, _, errb := cli(ctx, "siren", "remove", "dev_siren1", "-config", path); code != 1 || !strings.Contains(errb, "at least one siren") {
		t.Fatalf("%d %q", code, errb)
	}
	if code, _, errb := cli(ctx, "siren", "-config", path, "remove", "dev_nope"); code != 1 || !strings.Contains(errb, "not a siren") {
		t.Fatalf("%d %q", code, errb)
	}

	// A siren is named for the alerts' texts; a device that is no siren is not.
	code, out, errb = cli(ctx, "siren", "-config", path, "name", "-name", "Comptoir", "dev_siren1")
	if code != 0 || !strings.Contains(out, `dev_siren1: siren "Reserve" is now "Comptoir"`) || !strings.Contains(out, "systemctl restart") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	if n := readSite(t, path).SirenNames["dev_siren1"]; n != "Comptoir" {
		t.Fatalf("the name in the file: %q", n)
	}
	if code, out, _ := cli(ctx, "siren", "-config", path, "name", "dev_siren1", "-name", "Comptoir"); code != 0 || !strings.Contains(out, "already named") {
		t.Fatalf("the same name: %d %q", code, out)
	}
	if code, _, errb := cli(ctx, "siren", "-config", path, "name", "-name", "Porte", "dev_door"); code != 1 || !strings.Contains(errb, "dev_door is not a siren") {
		t.Fatalf("%d %q", code, errb)
	}
	if code, _, errb := cli(ctx, "siren", "-config", path, "name", "dev_siren1"); code != 1 || !strings.Contains(errb, "siren name is empty") {
		t.Fatalf("%d %q", code, errb)
	}
	if _, err := hubconfig.Load(path); err != nil {
		t.Fatalf("the file no longer loads: %v", err)
	}
}

func TestWithoutBrokerOrAdapter(t *testing.T) {
	ctx := context.Background()
	if code, _, errb := cli(ctx, "devices", "-config", writeConfig(t, "mqtt://127.0.0.1:1")); code != 1 || !strings.Contains(errb, "cannot reach the broker") {
		t.Fatalf("%d %q", code, errb)
	}

	broker := mqtttest.Start(t)
	path := writeConfig(t, broker.URL())
	if code, _, errb := cli(ctx, "devices", "-config", path); code != 1 || !strings.Contains(errb, "no device registry") {
		t.Fatalf("%d %q", code, errb)
	}
	if code, _, errb := cli(ctx, "pair", "-config", path, "-for", "30s"); code != 1 || !strings.Contains(errb, "did not answer") {
		t.Fatalf("%d %q", code, errb)
	}
	if code, _, errb := cli(ctx, "zone", "-config", path, "-name", "X", "-kind", "entry", "dev_x"); code != 1 || !strings.Contains(errb, "-force") {
		t.Fatalf("%d %q", code, errb)
	}
}

func TestAnInstallation(t *testing.T) {
	broker := mqtttest.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	path := writeConfig(t, broker.URL())
	a := startAdapter(t, ctx, broker.URL(), exampleDevices())

	// The example site agrees with the paired devices.
	code, out, errb := cli(ctx, "check", "-config", path)
	if code != 0 || !strings.Contains(out, "OK: 7 zone(s), 1 siren(s), 8 paired device(s)") {
		t.Fatalf("check: %d %q %q", code, out, errb)
	}
	code, out, _ = cli(ctx, "devices", "-config", path)
	for _, want := range []string{"Zigbee2MQTT: online, 8 device(s), pairing closed", `dev_door`, `zone "Entree" (entry)`, "dev_siren1", `siren "Reserve"`} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("devices: missing %q in %d %q", want, code, out)
		}
	}

	// Pairing a new contact.
	a.mu.Lock()
	a.join = []contract.RegistryDevice{dev("dev_k7p2", "contact", "battery"), dev("dev_s2", "siren", "link")}
	a.mu.Unlock()
	code, out, errb = cli(ctx, "pair", "-config", path, "-for", "1m")
	for _, want := range []string{"+ dev_k7p2", "next: safehome-install zone", "next: safehome-install siren -config " + path + " add dev_s2", "x refused TS0203: no profile", "network closed: 2 device(s) added"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("pair: missing %q in %d %q %q", want, code, out, errb)
		}
	}
	// Zigbee2MQTT refuses to open the network: the tool says so and fails.
	a.mu.Lock()
	a.refuse = "Coordinator is not started"
	a.mu.Unlock()
	code, _, errb = cli(ctx, "pair", "-config", path, "-for", "5m")
	if code != 1 || !strings.Contains(errb, "the network could not be opened: Coordinator is not started") {
		t.Fatalf("a refused opening: %d %q", code, errb)
	}
	// Zigbee2MQTT never answers: the adapter gives up and says so, and so does the tool.
	a.mu.Lock()
	a.refuse = "Zigbee2MQTT did not answer the request to open the network: is it running?"
	a.mu.Unlock()
	code, _, errb = cli(ctx, "pair", "-config", path, "-for", "1m")
	if code != 1 || !strings.Contains(errb, "the network could not be opened: Zigbee2MQTT did not answer") {
		t.Fatalf("an opening never confirmed: %d %q", code, errb)
	}
	if code, _, errb := cli(ctx, "pair", "-config", path, "-for", "11m"); code != 1 || !strings.Contains(errb, "-for") {
		t.Fatalf("pair beyond the bound: %d %q", code, errb)
	}

	// Unused until it gets a zone.
	if _, out, _ := cli(ctx, "check", "-config", path); !strings.Contains(out, "warning: dev_k7p2: paired") {
		t.Fatalf("check after pairing: %q", out)
	}
	if code, _, errb := cli(ctx, "zone", "dev_k7p2", "-config", path, "-name", "Fumee", "-kind", "24h_fire"); code != 1 || !strings.Contains(errb, "cannot serve") {
		t.Fatalf("a contact as a fire zone: %d %q", code, errb)
	}
	if code, _, errb := cli(ctx, "zone", "dev_k7p2", "-config", path, "-name", "Reserve", "-kind", "garage"); code != 1 || !strings.Contains(errb, "unknown zone kind") {
		t.Fatalf("unknown kind: %d %q", code, errb)
	}
	if code, _, errb := cli(ctx, "zone", "dev_k7p2", "-config", path, "-name", "", "-kind", "instant"); code != 1 || !strings.Contains(errb, "zone name is empty") {
		t.Fatalf("empty name: %d %q", code, errb)
	}
	code, out, errb = cli(ctx, "zone", "dev_k7p2", "-config", path, "-name", "Reserve", "-kind", "instant")
	if code != 0 || !strings.Contains(out, `dev_k7p2: zone "Reserve" (instant)`) {
		t.Fatalf("zone: %d %q %q", code, out, errb)
	}
	code, out, _ = cli(ctx, "zone", "dev_k7p2", "-config", path, "-name", "Reserve", "-kind", "entry")
	if code != 0 || !strings.Contains(out, `is now "Reserve" (entry)`) {
		t.Fatalf("zone change: %d %q", code, out)
	}
	if z := readSite(t, path).Zones["dev_k7p2"]; z != (install.Zone{Name: "Reserve", Kind: "entry"}) {
		t.Fatalf("zone in the file: %+v", z)
	}
	if code, out, _ := cli(ctx, "check", "-config", path); code != 0 || !strings.Contains(out, "OK: 8 zone(s)") {
		t.Fatalf("check after zone: %d %q", code, out)
	}

	// Sirens and unknown devices.
	if code, _, errb := cli(ctx, "siren", "-config", path, "add", "dev_door"); code != 1 || !strings.Contains(errb, "cannot sound") {
		t.Fatalf("a contact as a siren: %d %q", code, errb)
	}
	if code, out, _ := cli(ctx, "siren", "-config", path, "add", "dev_siren1"); code != 0 || !strings.Contains(out, "already a siren") {
		t.Fatalf("%d %q", code, out)
	}
	if code, _, errb := cli(ctx, "zone", "-config", path, "-name", "Coffre", "-kind", "24h_tamper", "dev_ups"); code != 1 || !strings.Contains(errb, "not paired") {
		t.Fatalf("an unknown device: %d %q", code, errb)
	}
	code, out, _ = cli(ctx, "zone", "-config", path, "-force", "-name", "Coffre", "-kind", "24h_tamper", "dev_ups")
	if code != 0 || !strings.Contains(out, "accepted unchecked") {
		t.Fatalf("-force: %d %q", code, out)
	}
	if code, out, _ := cli(ctx, "check", "-config", path); code != 0 || !strings.Contains(out, "warning: dev_ups") {
		t.Fatalf("check with a forced device: %d %q", code, out)
	}

	// Removal takes the device out of the configuration too.
	code, out, errb = cli(ctx, "remove", "-config", path, "dev_k7p2")
	if code != 0 || !strings.Contains(out, "removed from the network") || !strings.Contains(out, `zone "Reserve" removed`) {
		t.Fatalf("remove: %d %q %q", code, out, errb)
	}
	if _, ok := readSite(t, path).Zones["dev_k7p2"]; ok {
		t.Fatal("the zone of the removed device is still in the file")
	}
	if code, _, errb := cli(ctx, "remove", "-config", path, "dev_k7p2"); code != 1 || !strings.Contains(errb, "no such device") {
		t.Fatalf("second removal: %d %q", code, errb)
	}
	// The only siren cannot be removed: nothing is sent, the device stays paired.
	if code, _, errb := cli(ctx, "remove", "-config", path, "dev_siren1"); code != 1 || !strings.Contains(errb, "cannot be removed yet") {
		t.Fatalf("remove the only siren: %d %q", code, errb)
	}
	a.mu.Lock()
	still := slices.ContainsFunc(a.devices, func(d contract.RegistryDevice) bool { return d.DeviceID == "dev_siren1" })
	a.mu.Unlock()
	if !still {
		t.Fatal("the only siren was removed from the network")
	}
	// With its replacement in place, it can go.
	if code, out, errb := cli(ctx, "siren", "-config", path, "-force", "add", "-name", "Vitrine", "dev_siren2"); code != 0 ||
		!strings.Contains(out, "now a siren") || !strings.Contains(out, `dev_siren2: siren named "Vitrine"`) {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	if code, _, errb := cli(ctx, "siren", "-config", path, "-force", "add", "-name", " ", "dev_siren3"); code != 1 ||
		!strings.Contains(errb, "siren name is empty") || slices.Contains(readSite(t, path).SirenIDs, "dev_siren3") {
		t.Fatalf("a bad name is refused before the siren is added: %d %q", code, errb)
	}
	code, out, errb = cli(ctx, "remove", "-config", path, "dev_siren1")
	if code != 0 || !strings.Contains(out, "siren removed from the configuration") {
		t.Fatalf("remove a siren: %d %q %q", code, out, errb)
	}
	if code, out, errb := cli(ctx, "siren", "-config", path, "remove", "dev_siren2"); code != 1 || !strings.Contains(errb, "at least one siren") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	if s := readSite(t, path); !maps.Equal(s.SirenNames, map[string]string{"dev_siren2": "Vitrine"}) {
		t.Fatalf("a siren removed from the network leaves its name behind: %v", s.SirenNames)
	}
	if _, err := hubconfig.Load(path); err != nil {
		t.Fatalf("the file no longer loads: %v", err)
	}
}

func TestWatchShowsEventsWithZoneNamesAndNoCode(t *testing.T) {
	broker := mqtttest.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := writeConfig(t, broker.URL())
	a := startAdapter(t, ctx, broker.URL(), exampleDevices())

	watchCtx, stopWatch := context.WithCancel(ctx)
	var out drivertest.SyncBuffer
	done := make(chan int)
	go func() { done <- run(watchCtx, []string{"watch", "-config", path}, &out, &out) }()

	// Publish until the watcher is subscribed and has shown both events.
	want := []string{`dev_door     Entree           contact = "open"`, `dev_keypad   -                keypad = "disarm"`}
	deadline := time.Now().Add(10 * time.Second)
	for !containsAll(out.String(), want) && time.Now().Before(deadline) {
		a.publish(ctx, contract.DeviceEventTopic(site, "dev_door"), contract.TypeDeviceEvent,
			contract.DeviceEventData{DeviceID: "dev_door", Capability: "contact", Value: json.RawMessage(`"open"`)}, false)
		a.publish(ctx, contract.DeviceEventTopic(site, "dev_keypad"), contract.TypeDeviceEvent,
			contract.DeviceEventData{DeviceID: "dev_keypad", Capability: "keypad", Value: json.RawMessage(`"disarm"`), Code: "482916"}, false)
		time.Sleep(100 * time.Millisecond)
	}
	stopWatch()
	code := <-done
	if code != 0 || !containsAll(out.String(), want) {
		t.Fatalf("watch: %d %q", code, out.String())
	}
	if strings.Contains(out.String(), "482916") {
		t.Fatal("a keypad code was shown")
	}
}

func containsAll(s string, parts []string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
