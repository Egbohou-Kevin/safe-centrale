package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

const fakeSSH = "SAFEHOME_FAKE_SSH"

// TestMain lets the test binary play ssh: it grants the tunnel and holds it until
// stopped.
func TestMain(m *testing.M) {
	if os.Getenv(fakeSSH) == "1" {
		fmt.Fprintln(os.Stderr, "debug1: remote forward success for: listen localhost:22001, connect localhost:22")
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		<-stop
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func writeConfig(t *testing.T, broker, state, extra string) string {
	t.Helper()
	dir := t.TempDir()
	key, kh := filepath.Join(dir, "maint.key"), filepath.Join(dir, "known_hosts")
	os.WriteFile(key, []byte("not a real key"), 0o600)
	os.WriteFile(kh, []byte("bastion.example ssh-ed25519 AAAA"), 0o600)
	body := fmt.Sprintf(`{
  "site": %q, "broker_url": %q, "client_id": "maint-test-%d", "state_dir": %q, "log_level": "debug",
  "ssh": %q, "key_file": %q, "known_hosts": %q,
  "bastion": {"host": "bastion.example", "user": "tunnel", "remote_port": 22001}%s
}`, site, broker, time.Now().UnixNano(), state, os.Args[0], key, kh, extra)
	path := filepath.Join(dir, "maint.json")
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
	if code, out, _ := cli("version"); code != 0 || !strings.Contains(out, "safehome-maint") {
		t.Fatalf("%d %q", code, out)
	}
	for _, args := range [][]string{{}, {"fly"}, {"run"}, {"check", "-nonsense"}} {
		if code, _, errb := cli(args...); code != 2 || errb == "" {
			t.Errorf("%v: %d %q", args, code, errb)
		}
	}
	path := writeConfig(t, "mqtt://127.0.0.1:1", t.TempDir(), "")
	if code, out, errb := cli("check", "-config", path); code != 0 || !strings.Contains(out, "tunnel to tunnel@bastion.example:22, port 22001 there, 1h0m0s at most") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	os.Remove(filepath.Join(filepath.Dir(path), "maint.key"))
	if code, _, errb := cli("check", "-config", path); code != 1 || !strings.Contains(errb, "the hub's key is missing") {
		t.Fatalf("%d %q", code, errb)
	}
	off := writeConfig(t, "mqtt://127.0.0.1:1", t.TempDir(), `, "enabled": false`)
	if code, out, _ := cli("check", "-config", off); code != 0 || !strings.Contains(out, "switched off") {
		t.Fatalf("%d %q", code, out)
	}
	if code, _, errb := cli("check", "-config", filepath.Join(t.TempDir(), "none.json")); code != 1 || !strings.Contains(errb, "configuration error") {
		t.Fatalf("%d %q", code, errb)
	}
	// The example checks, but for the keys, which only exist on a hub.
	if code, _, errb := cli("check", "-config", filepath.Join("..", "..", "config", "maint.example.json")); !strings.Contains(errb, "/etc/safehome/maint.key") {
		t.Fatalf("%d %q", code, errb)
	}
}

func TestASecondServiceOnTheSameStateIsRefused(t *testing.T) {
	state := t.TempDir()
	running, err := pidfile.Lock(state, pidName) // a copy that runs holds the pid file
	if err != nil {
		t.Fatal(err)
	}
	defer running()
	if code, _, errb := cli("run", "-config", writeConfig(t, "mqtt://127.0.0.1:1", state, "")); code != 1 || !strings.Contains(errb, "already using") {
		t.Fatalf("%d %q", code, errb)
	}
}

// cloudSide plays what the agent republishes from the cloud, and reads maint.session.
type cloudSide struct {
	mu     sync.Mutex
	steps  []contract.MaintSessionData
	client *mqtt.Client
	ids    *contract.IDGenerator
}

func (c *cloudSide) send(t *testing.T, typ string, data any) {
	t.Helper()
	env, _ := contract.NewEnvelope(site, "cloud", 1, c.ids.New(), time.Now(), typ, data)
	b, _ := json.Marshal(env)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.client.Publish(ctx, mqtt.Message{Topic: contract.MaintCmdTopic(site), Payload: b, QoS: 1}); err != nil {
		t.Fatal(err)
	}
}

func (c *cloudSide) statuses() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.steps {
		out = append(out, strings.TrimSuffix(s.Status+"/"+s.Reason, "/"))
	}
	return strings.Join(out, " ")
}

func TestATunnelEndToEnd(t *testing.T) {
	t.Setenv(fakeSSH, "1")
	broker := mqtttest.Start(t, "safehome-maint")
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state, "")

	c := &cloudSide{ids: contract.NewIDGenerator()}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mqtt.Dial(ctx, mqtt.Options{
		Username: mqtttest.Peer, Password: mqtttest.Password(mqtttest.Peer),
		BrokerURL: broker.URL(), ClientID: fmt.Sprintf("agent-%d", time.Now().UnixNano()),
		Subscriptions: []mqtt.Subscription{{Filter: contract.MaintSessionTopic(site), QoS: 1}},
		OnMessage: func(m mqtt.Message) {
			env, _ := contract.ParseEnvelope(m.Payload)
			if err := schematest.Check("maint-session.json", env.Data); err != nil {
				t.Error(err)
			}
			d, _ := contract.DecodeData[contract.MaintSessionData](env)
			c.mu.Lock()
			c.steps = append(c.steps, d)
			c.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	if err := client.Await(ctx); err != nil {
		t.Fatal(err)
	}
	c.client = client

	logs := drivertest.Logs(t)
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(runCtx, []string{"run", "-config", cfg}, &drivertest.SyncBuffer{}, logs) }()
	drivertest.WaitFor(t, "the service", func() bool { return strings.Contains(logs.String(), "mqtt connected") })
	time.Sleep(300 * time.Millisecond) // its subscription

	c.send(t, contract.TypeMaintOpen, contract.MaintOpenData{Session: "m1", User: "kevin", Reason: "modem silent", DurationS: 1800,
		Exp: contract.TS(time.Now().Add(time.Minute))})
	drivertest.WaitFor(t, "open", func() bool { return c.statuses() == "opening open" })
	c.send(t, contract.TypeMaintClose, contract.MaintCloseData{Session: "m1"})
	drivertest.WaitFor(t, "closed", func() bool { return c.statuses() == "opening open closed/stopped" })
	drivertest.WaitFor(t, "ssh stopped", func() bool { return strings.Contains(logs.String(), "ssh stopped") })

	stop()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d\n%s", code, logs.String())
	}
	for _, want := range []string{"user=kevin", `reason="modem silent"`, "granted the tunnel"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the journal misses %q:\n%s", want, logs.String())
		}
	}
}
