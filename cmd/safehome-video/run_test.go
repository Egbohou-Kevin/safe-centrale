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

	"safehome/internal/adapter/go2rtc/go2rtctest"
	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/mqtt/mqtttest"
	"safehome/internal/contract"
	"safehome/internal/schematest"
	"safehome/internal/siteconfig"
)

func TestMain(m *testing.M) {
	go2rtctest.RunIfFake() // the test binary also plays go2rtc
	os.Exit(m.Run())
}

const site = "tg-lom-0001"

func writeConfig(t *testing.T, broker, go2rtcPath, state, extra string) string {
	t.Helper()
	body := fmt.Sprintf(`{
  "site": %q, "broker_url": %q, "client_id": "video-test-%d", "state_dir": %q, "log_level": "debug",
  "go2rtc": %q, "start_timeout": "3s", "stall_after": "3s",
  "cameras": [
    {"id": "cam_caisse", "name": "Caisse", "source": "rtsp://admin:camPW99@192.168.1.20/stream2"},
    {"id": "cam_cour", "name": "Cour", "source": "rtsp://admin:camPW99@deadcam.local/stream2"}
  ]%s
}`, site, broker, time.Now().UnixNano(), state, go2rtcPath, extra)
	path := filepath.Join(t.TempDir(), "video.json")
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
	if code, out, _ := cli("version"); code != 0 || !strings.Contains(out, "safehome-video") {
		t.Fatalf("%d %q", code, out)
	}
	for _, args := range [][]string{{}, {"fly"}, {"run"}, {"check", "-nonsense"}} {
		if code, _, errb := cli(args...); code != 2 || errb == "" {
			t.Errorf("%v: %d %q", args, code, errb)
		}
	}
	code, out, _ := cli("check", "-config", writeConfig(t, "mqtt://127.0.0.1:1", os.Args[0], t.TempDir(), `, "uplink": "mobile"`))
	if code != 0 || !strings.Contains(out, "2 camera(s), mobile data, 1024 MB a month, sessions of 5m0s at most") || strings.Contains(out, "camPW") {
		t.Fatalf("%d %q", code, out)
	}
	if code, _, errb := cli("check", "-config", writeConfig(t, "mqtt://127.0.0.1:1", filepath.Join(t.TempDir(), "go2rtc"), t.TempDir(), "")); code != 1 || !strings.Contains(errb, "go2rtc is not installed") {
		t.Fatalf("%d %q", code, errb)
	}
	if code, _, errb := cli("check", "-config", filepath.Join(t.TempDir(), "none.json")); code != 1 || !strings.Contains(errb, "configuration error") {
		t.Fatalf("%d %q", code, errb)
	}
}

func TestTheConfigExampleChecks(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "video.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The example points at /usr/local/bin/go2rtc: here, at the fake.
	data = bytes.Replace(data, []byte(`"/usr/local/bin/go2rtc"`), []byte(fmt.Sprintf("%q", os.Args[0])), 1)
	path := filepath.Join(t.TempDir(), "video.json")
	os.WriteFile(path, data, 0o600)
	if code, out, errb := cli("check", "-config", path); code != 0 {
		t.Fatalf("%d %q %q", code, out, errb)
	}
}

func TestASecondServiceOnTheSameStateIsRefused(t *testing.T) {
	state := t.TempDir()
	cfg := writeConfig(t, "mqtt://127.0.0.1:1", os.Args[0], state, "")
	os.WriteFile(filepath.Join(state, pidName), []byte(fmt.Sprintf("%d\n", os.Getppid())), 0o600)
	if code, _, errb := cli("run", "-config", cfg); code != 1 || !strings.Contains(errb, "already using") {
		t.Fatalf("%d %q", code, errb)
	}
}

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

// cloud is the test's MQTT client: it plays the cloud (through the agent) and records
// what the service publishes.
type cloud struct {
	mu     sync.Mutex
	msgs   []contract.Envelope
	client *mqtt.Client
	ids    *contract.IDGenerator
}

func (c *cloud) sessions() []contract.VideoSessionData {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []contract.VideoSessionData
	for _, e := range c.msgs {
		if e.Type == contract.TypeVideoSession {
			d, _ := contract.DecodeData[contract.VideoSessionData](e)
			out = append(out, d)
		}
	}
	return out
}

func (c *cloud) has(session, status, reason string) bool {
	for _, s := range c.sessions() {
		if s.Session == session && s.Status == status && s.Reason == reason {
			return true
		}
	}
	return false
}

func (c *cloud) status() contract.VideoStatusData {
	c.mu.Lock()
	defer c.mu.Unlock()
	var d contract.VideoStatusData
	for _, e := range c.msgs {
		if e.Type == contract.TypeVideoStatus {
			d, _ = contract.DecodeData[contract.VideoStatusData](e)
		}
	}
	return d
}

func (c *cloud) send(t *testing.T, typ string, data any) {
	t.Helper()
	env, err := contract.NewEnvelope(site, "cloud", 1, c.ids.New(), time.Now(), typ, data)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	if err := c.client.Publish(context.Background(), mqtt.Message{Topic: contract.VideoCmdTopic(site), Payload: b, QoS: 1}); err != nil {
		t.Fatal(err)
	}
}

func (c *cloud) request(t *testing.T, session, camera string) {
	t.Helper()
	c.send(t, contract.TypeVideoRequest, contract.VideoRequestData{
		Session: session, Camera: camera, Reason: contract.VideoForAlarm, User: "u_owner", Incident: "inc1",
		PushURL: "rtmps://relay.safehome.example/live/" + session, Token: "tok-" + session + "-secret",
		Exp: contract.TS(time.Now().Add(30 * time.Second)),
	})
}

func waitFor(t *testing.T, logs *syncBuffer, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s\nlogs:\n%s", what, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTheServiceEndToEndWithASimulatedGo2rtc(t *testing.T) {
	go2rtctest.Enable(t)
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), os.Args[0], state, `, "uplink": "mobile", "quota_mb": 100`)

	c := &cloud{ids: contract.NewIDGenerator()}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mqtt.Dial(ctx, mqtt.Options{
		BrokerURL: broker.URL(), ClientID: fmt.Sprintf("cloud-%d", time.Now().UnixNano()),
		Subscriptions: []mqtt.Subscription{{Filter: "safehome/" + site + "/video/#", QoS: 1, NoLocal: true}},
		OnMessage: func(m mqtt.Message) {
			if env, err := contract.ParseEnvelope(m.Payload); err == nil {
				c.mu.Lock()
				c.msgs = append(c.msgs, env)
				c.mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Await(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	c.client = client

	logs := &syncBuffer{}
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(runCtx, []string{"run", "-config", cfg}, &syncBuffer{}, logs) }()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}()

	// The cameras are checked: the dead one does not answer (the fake has no camera,
	// so neither answers on the network).
	waitFor(t, logs, "the status", func() bool {
		st := c.status()
		return len(st.Cameras) == 2 && st.Uplink == contract.UplinkMobile && st.QuotaMB == 100 && st.MaxS == 300
	})

	// A session: starting, live, then stopped by the cloud.
	c.request(t, "s1", "cam_caisse")
	waitFor(t, logs, "live", func() bool { return c.has("s1", contract.VideoLive, "") })
	waitFor(t, logs, "one session in the status", func() bool { return c.status().Active == 1 })
	time.Sleep(100 * time.Millisecond)
	c.send(t, contract.TypeVideoStop, contract.VideoStopData{Session: "s1"})
	waitFor(t, logs, "ended", func() bool { return c.has("s1", contract.VideoEnded, contract.VideoStopped) })
	var ended contract.VideoSessionData
	for _, s := range c.sessions() {
		if s.Status == contract.VideoEnded {
			ended = s
		}
	}
	if ended.Bytes <= 0 {
		t.Fatalf("%+v", ended)
	}

	// The dead camera fails at once.
	c.request(t, "s2", "cam_cour")
	waitFor(t, logs, "the dead camera", func() bool { return c.has("s2", contract.VideoFailed, contract.VideoCameraUnreachable) })

	// Everything published follows the contract.
	c.mu.Lock()
	msgs := append([]contract.Envelope(nil), c.msgs...)
	c.mu.Unlock()
	files := map[string]string{contract.TypeVideoSession: "video-session.json", contract.TypeVideoStatus: "video-status.json"}
	for _, e := range msgs {
		if file, ok := files[e.Type]; ok && e.Src == "video" {
			if err := schematest.Check(file, e.Data); err != nil {
				t.Errorf("%s: %v", e.Type, err)
			}
		}
	}

	// A clean stop leaves no go2rtc behind and removes the pid file; the logs never
	// show the token nor the cameras' password, although the fake go2rtc printed them.
	stop()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the service did not stop")
	}
	if _, err := os.Stat(filepath.Join(state, pidName)); !os.IsNotExist(err) {
		t.Fatal("the pid file must be removed")
	}
	if l := logs.String(); strings.Contains(l, "secret") || strings.Contains(l, "camPW") || !strings.Contains(l, "url=***") {
		t.Fatalf("logs:\n%s", l)
	}
}

// publishApplied sends config.applied as the hub does once it runs with a version,
// retained.
func publishApplied(t *testing.T, broker string, version int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := mqtt.Dial(ctx, mqtt.Options{BrokerURL: broker, ClientID: fmt.Sprintf("hub-%d", time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if err := c.Await(ctx); err != nil {
		t.Fatal(err)
	}
	env, _ := contract.NewEnvelope(site, "config", uint64(version), contract.NewIDGenerator().New(), time.Now(),
		contract.TypeConfigApplied, contract.ConfigAppliedData{Version: version, Status: contract.ConfigApplied})
	b, _ := json.Marshal(env)
	if err := c.Publish(ctx, mqtt.Message{Topic: contract.ConfigAppliedTopic(site), Payload: b, QoS: 1, Retain: true}); err != nil {
		t.Fatal(err)
	}
}

func TestTheServiceRestartsForANewConfigurationFromTheCloud(t *testing.T) {
	go2rtctest.Enable(t)
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), os.Args[0], state, `, "uplink": "mobile"`)
	start := func(logs *syncBuffer) (context.CancelFunc, chan int) {
		ctx, stop := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() { done <- run(ctx, []string{"run", "-config", cfg}, &syncBuffer{}, logs) }()
		waitFor(t, logs, "the service to start", func() bool { return strings.Contains(logs.String(), "msg=starting") })
		return stop, done
	}
	logs := &syncBuffer{}
	stop, done := start(logs)
	time.Sleep(300 * time.Millisecond)

	quota, longest := 200, 120
	siteconfig.Write(state, contract.ConfigSetData{Version: 3, Video: &contract.ConfigVideo{QuotaMB: &quota, MaxS: &longest}})
	publishApplied(t, broker.URL(), 3)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d\nlogs:\n%s", code, logs.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no restart\nlogs:\n%s", logs.String())
	}
	stop()

	logs = &syncBuffer{}
	stop, done = start(logs)
	time.Sleep(500 * time.Millisecond)
	select {
	case code := <-done:
		t.Fatalf("restarted again (exit %d): it would loop\nlogs:\n%s", code, logs.String())
	default:
	}
	stop()
	<-done
	if !strings.Contains(logs.String(), "site_config=3") {
		t.Fatalf("logs:\n%s", logs.String())
	}
	if code, out, _ := cli("check", "-config", cfg); code != 0 || !strings.Contains(out, "200 MB a month, sessions of 2m0s") || !strings.Contains(out, "version 3") {
		t.Fatalf("%d %q", code, out)
	}
	// On a fixed line, the allowance from the cloud is left out, and check says so.
	fixed := writeConfig(t, broker.URL(), os.Args[0], state, "")
	if code, out, _ := cli("check", "-config", fixed); code != 0 || !strings.Contains(out, "fixed line") || !strings.Contains(out, "note: ") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestAnAlarmIsPhotographed(t *testing.T) {
	go2rtctest.Enable(t)
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), os.Args[0], state, `, "uplink": "mobile"`)

	var mu sync.Mutex
	var photos []contract.PhotoTakenData
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mqtt.Dial(ctx, mqtt.Options{
		BrokerURL: broker.URL(), ClientID: fmt.Sprintf("engine-%d", time.Now().UnixNano()),
		Subscriptions: []mqtt.Subscription{{Filter: contract.PhotoTakenTopic(site), QoS: 1}},
		OnMessage: func(m mqtt.Message) {
			env, _ := contract.ParseEnvelope(m.Payload)
			if err := schematest.Check("photo-taken.json", env.Data); err != nil {
				t.Error(err)
			}
			d, _ := contract.DecodeData[contract.PhotoTakenData](env)
			mu.Lock()
			photos = append(photos, d)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	if err := client.Await(ctx); err != nil {
		t.Fatal(err)
	}

	logs := &syncBuffer{}
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(runCtx, []string{"run", "-config", cfg}, &syncBuffer{}, logs) }()
	defer func() { stop(); <-done }()
	waitFor(t, logs, "the service to listen", func() bool { return strings.Contains(logs.String(), "msg=starting") })
	time.Sleep(300 * time.Millisecond) // its subscriptions

	env, _ := contract.NewEnvelope(site, "engine", 1, contract.NewIDGenerator().New(), time.Now(), contract.TypeAlarmTriggered,
		contract.TriggeredData{Incident: "inc1", DeviceID: "dev_door", Capability: contract.CapContact, Zone: "entrance"})
	b, _ := json.Marshal(env)
	if err := client.Publish(ctx, mqtt.Message{Topic: contract.AlarmEventTopic(site), Payload: b, QoS: 1}); err != nil {
		t.Fatal(err)
	}
	// Economy on mobile data: one picture per camera, 480 px. The second camera is dead.
	waitFor(t, logs, "the photos", func() bool { return strings.Contains(logs.String(), "photos of the alarm taken") })
	waitFor(t, logs, "photo.taken", func() bool { mu.Lock(); defer mu.Unlock(); return len(photos) > 0 })
	time.Sleep(100 * time.Millisecond) // nothing more comes
	mu.Lock()
	defer mu.Unlock()
	if len(photos) != 1 || photos[0].Camera != "cam_caisse" || photos[0].Width != 480 || photos[0].Incident != "inc1" {
		t.Fatalf("%+v\nlogs:\n%s", photos, logs.String())
	}
	fi, err := os.Stat(filepath.Join(state, "photos", photos[0].Photo+".jpg"))
	if err != nil || fi.Size() != photos[0].Bytes || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	if strings.Contains(logs.String(), "camPW") {
		t.Fatal("a camera's password in the logs")
	}
}
