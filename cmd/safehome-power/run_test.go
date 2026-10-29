package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/mqtt/mqtttest"
	"safehome/internal/driver/drivertest"
	"safehome/internal/powerconfig"
)

func cli(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "power.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Without the module (this computer), check says the configuration is good and that
// the module cannot be reached, without failing: the installation comes before it.
func TestCheckSaysTheConfigurationAndTheModule(t *testing.T) {
	cfg := writeConfig(t, `{"site":"tg-lom-0001","state_dir":"`+t.TempDir()+`","device_id":"dev_hubpower","gpio_chip":"no-such-chip"}`)
	code, out, errs := cli("check", "-config", cfg)
	if code != 0 || !strings.Contains(out, "configuration OK: site tg-lom-0001, device dev_hubpower") ||
		!strings.Contains(errs, "cannot be reached") {
		t.Fatalf("code %d\n%s\n%s", code, out, errs)
	}
}

func TestABadConfigurationIsRefused(t *testing.T) {
	cfg := writeConfig(t, `{"site":"tg-lom-0001","state_dir":"/s"}`)
	if code, _, errs := cli("check", "-config", cfg); code != 1 || !strings.Contains(errs, "configuration error") {
		t.Fatalf("code %d: %s", code, errs)
	}
}

// The service does not start without its module: systemd starts it again later.
func TestTheServiceNeedsItsModule(t *testing.T) {
	cfg := writeConfig(t, `{"site":"tg-lom-0001","state_dir":"`+t.TempDir()+`","device_id":"dev_hubpower","gpio_chip":"no-such-chip"}`)
	if code, _, errs := cli("run", "-config", cfg); code != 1 || !strings.Contains(errs, "cannot reach the battery module") {
		t.Fatalf("code %d: %s", code, errs)
	}
}

// fakeModule is a battery module on mains, three quarters full.
type fakeModule struct{}

func (fakeModule) MainsLost(context.Context) (bool, error) { return false, nil }
func (fakeModule) Gauge(context.Context) (int, int, error) { return 3900, 75, nil }
func (fakeModule) Cut(context.Context) error               { return nil }
func (fakeModule) Close() error                            { return nil }

// The service on the hub's broker, under its own name and with its rights alone
// (deploy/mosquitto/safehome.acl): it tells the mains and the battery, each as an event
// and as the retained state the hub reads when it starts. A right taken from the ACL
// leaves a topic empty here.
func TestTheServiceTellsTheHubOnItsBroker(t *testing.T) {
	broker := mqtttest.Start(t, "safehome-power")
	const dev = "safehome/tg-lom-0001/device/dev_hubpower/"
	path := writeConfig(t, fmt.Sprintf(`{"site":"tg-lom-0001","broker_url":%q,"client_id":"power-test-%d","state_dir":%q,"device_id":"dev_hubpower","gpio_chip":"no-such-chip"}`,
		broker.URL(), time.Now().UnixNano(), t.TempDir()))
	cfg, err := powerconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var mu sync.Mutex
	heard := map[string][]string{} // listener: topics
	listen := func(who string) {
		c, err := mqtt.Dial(ctx, mqtt.Options{
			BrokerURL: broker.URL(), ClientID: fmt.Sprintf("%s-%d", who, time.Now().UnixNano()),
			Username: mqtttest.Peer, Password: mqtttest.Password(mqtttest.Peer), Ephemeral: true,
			Subscriptions: []mqtt.Subscription{{Filter: dev + "#", QoS: 1}},
			OnMessage:     func(m mqtt.Message) { mu.Lock(); heard[who] = append(heard[who], m.Topic); mu.Unlock() },
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close(context.Background()) })
		if err := c.Await(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond) // its subscription
	}
	has := func(who string, topics ...string) func() bool {
		return func() bool {
			mu.Lock()
			defer mu.Unlock()
			return !slices.ContainsFunc(topics, func(tp string) bool { return !slices.Contains(heard[who], tp) })
		}
	}
	listen("live")

	logs := drivertest.Logs(t)
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- runService(runCtx, cfg, func(powerconfig.Board) (module, error) { return fakeModule{}, nil }, logs)
	}()
	drivertest.WaitFor(t, "the events and the states", has("live",
		dev+"event", dev+"state/mains", dev+"state/battery"))
	// A client that comes later, as the hub when it starts, finds the states kept.
	listen("later")
	drivertest.WaitFor(t, "the retained states", has("later", dev+"state/mains", dev+"state/battery"))

	stop()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d\n%s", code, logs.String())
	}
	if strings.Contains(logs.String(), "refused") {
		t.Errorf("the broker refused the service:\n%s", logs.String())
	}
}
