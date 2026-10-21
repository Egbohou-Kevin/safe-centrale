package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	"safehome/internal/schematest"
)

const site = "tg-lom-0001"

// writeConfig writes a configuration and its secret in a temporary folder.
func writeConfig(t *testing.T, broker, cloudURL, caFile, state string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cloud.secret"), []byte("pw-of-the-site\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{
  "site": %q, "broker_url": %q, "client_id": "agent-test-%d", "state_dir": %q, "log_level": "debug",
  "cloud": {"urls": [%q], "password_file": "cloud.secret", "ca_file": %q}
}`, site, broker, time.Now().UnixNano(), state, cloudURL, caFile)
	path := filepath.Join(dir, "agent.json")
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
	if code, out, _ := cli("version"); code != 0 || !strings.Contains(out, "safehome-agent") {
		t.Fatalf("%d %q", code, out)
	}
	for _, args := range [][]string{{}, {"fly"}, {"run"}, {"check", "-nonsense"}} {
		if code, _, errb := cli(args...); code != 2 || errb == "" {
			t.Errorf("%v: %d %q", args, code, errb)
		}
	}
	code, out, _ := cli("check", "-config", writeConfig(t, "mqtt://127.0.0.1:1", "mqtts://cloud.example:8883", "", t.TempDir()))
	if code != 0 || !strings.Contains(out, "mqtts://cloud.example:8883 as tg-lom-0001") || strings.Contains(out, "pw-of") {
		t.Fatalf("%d %q", code, out)
	}
	if code, _, errb := cli("check", "-config", filepath.Join(t.TempDir(), "none.json")); code != 1 || !strings.Contains(errb, "configuration error") {
		t.Fatalf("%d %q", code, errb)
	}
}

func TestTheConfigExampleChecksWithItsSecret(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "agent.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(filepath.Join("..", "..", "config", "cloud.secret.example"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cloud.secret"), secret, 0o600)
	os.WriteFile(filepath.Join(dir, "agent.json"), data, 0o600)
	if code, out, errb := cli("check", "-config", filepath.Join(dir, "agent.json")); code != 0 {
		t.Fatalf("%d %q %q", code, out, errb)
	}
}

func TestASecondAgentOnTheSameStateIsRefused(t *testing.T) {
	state := t.TempDir()
	cfg := writeConfig(t, "mqtt://127.0.0.1:1", "mqtts://cloud.example:8883", "", state)
	os.WriteFile(filepath.Join(state, pidName), []byte(fmt.Sprintf("%d\n", os.Getppid())), 0o600)
	if code, _, errb := cli("run", "-config", cfg); code != 1 || !strings.Contains(errb, "already using") {
		t.Fatalf("%d %q", code, errb)
	}
}

// side is a test client on one broker: it plays the hub's programs, or the cloud.
type side struct {
	t      *testing.T
	mu     sync.Mutex
	msgs   []mqtt.Message
	client *mqtt.Client
	ids    *contract.IDGenerator
	seq    uint64
}

func newSide(t *testing.T, ctx context.Context, url, caFile string) *side {
	t.Helper()
	s := &side{t: t, ids: contract.NewIDGenerator()}
	client, err := mqtt.Dial(ctx, mqtt.Options{
		BrokerURL: url, CAFile: caFile, ClientID: fmt.Sprintf("side-%d", time.Now().UnixNano()),
		// NoLocal: a side records what the agent publishes, not its own messages.
		Subscriptions: []mqtt.Subscription{{Filter: "safehome/" + site + "/#", QoS: 1, NoLocal: true}},
		OnMessage:     func(m mqtt.Message) { s.mu.Lock(); s.msgs = append(s.msgs, m); s.mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Await(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(context.Background()) })
	s.client = client
	return s
}

// of returns the envelopes of a type received, in order.
func (s *side) of(typ string) []contract.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []contract.Envelope
	for _, m := range s.msgs {
		if env, err := contract.ParseEnvelope(m.Payload); err == nil && env.Type == typ {
			out = append(out, env)
		}
	}
	return out
}

func (s *side) lastStatus() contract.AgentStatusData {
	st := s.of(contract.TypeAgentStatus)
	if len(st) == 0 {
		return contract.AgentStatusData{}
	}
	d, _ := contract.DecodeData[contract.AgentStatusData](st[len(st)-1])
	return d
}

func (s *side) publish(topic, src, typ string, data any) string {
	s.t.Helper()
	s.seq++
	id := s.ids.New()
	env, err := contract.NewEnvelope(site, src, s.seq, id, time.Now(), typ, data)
	if err != nil {
		s.t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.client.Publish(ctx, mqtt.Message{Topic: topic, Payload: b, QoS: 1}); err != nil {
		s.t.Fatal(err)
	}
	return id
}

func (s *side) has(typ, id string) bool {
	for _, env := range s.of(typ) {
		if env.ID == id {
			return true
		}
	}
	return false
}

// internet stands between the agent and the cloud's broker: cut drops the
// connections and refuses new ones, as a dead link would, while the broker and the
// test's own cloud client go on.
type internet struct {
	l     net.Listener
	mu    sync.Mutex
	down  bool
	conns []net.Conn
}

func startInternet(t *testing.T, target string) *internet {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	n := &internet{l: l}
	t.Cleanup(func() { l.Close(); n.cut() })
	go func() {
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			n.mu.Lock()
			down := n.down
			n.mu.Unlock()
			out, err := net.Dial("tcp", target)
			if down || err != nil {
				in.Close()
				continue
			}
			n.mu.Lock()
			n.conns = append(n.conns, in, out)
			n.mu.Unlock()
			go func() { io.Copy(out, in); out.Close() }()
			go func() { io.Copy(in, out); in.Close() }()
		}
	}()
	return n
}

// url is the cloud's address through this link (the certificate is for localhost).
func (n *internet) url() string {
	return fmt.Sprintf("mqtts://localhost:%d", n.l.Addr().(*net.TCPAddr).Port)
}

func (n *internet) cut() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.down = true
	for _, c := range n.conns {
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetLinger(0)
		}
		c.Close()
	}
	n.conns = nil
}

func (n *internet) restore() { n.mu.Lock(); n.down = false; n.mu.Unlock() }

func TestTheAgentEndToEndBetweenTheHubAndATLSCloud(t *testing.T) {
	hubBroker := mqtttest.Start(t)
	files := mqtttest.NewTLSFiles(t)
	cloudBroker := mqtttest.StartWith(t, mqtttest.Options{TLS: &files})
	link := startInternet(t, strings.TrimPrefix(cloudBroker.TLSURL(), "mqtts://"))
	state := t.TempDir()
	cfg := writeConfig(t, hubBroker.URL(), link.url(), files.CA, state)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	hub := newSide(t, ctx, hubBroker.URL(), "")
	cloud := newSide(t, ctx, cloudBroker.TLSURL(), files.CA)

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

	// The agent tells the hub the internet is up, over TLS.
	drivertest.WaitFor(t, "the link up on the hub", func() bool {
		st := hub.lastStatus()
		return st.Uplink == contract.UplinkUp && st.Transport == "mqtts"
	})

	// An alarm goes up to the cloud.
	triggered := contract.TriggeredData{Incident: "inc1", DeviceID: "dev_door", Capability: contract.CapContact, Zone: "Entree"}
	first := hub.publish(contract.AlarmEventTopic(site), "engine.alarm", contract.TypeAlarmTriggered, triggered)
	drivertest.WaitFor(t, "the alarm on the cloud", func() bool { return cloud.has(contract.TypeAlarmTriggered, first) })

	// The app arms the alarm from the cloud: the order reaches the hub, and so does a
	// new configuration. A configuration without version does not, and the agent says
	// so.
	order := cloud.publish(contract.AlarmCmdTopic(site), "cloud", contract.TypeAlarmCommand,
		contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp, User: "u_owner"})
	drivertest.WaitFor(t, "the order on the hub", func() bool { return hub.has(contract.TypeAlarmCommand, order) })
	conf := cloud.publish(contract.ConfigSetTopic(site), "cloud", contract.TypeConfigSet, contract.ConfigSetData{Version: 2, EntryDelayS: 30})
	drivertest.WaitFor(t, "the configuration on the hub", func() bool { return hub.has(contract.TypeConfigSet, conf) })
	cloud.publish(contract.ConfigSetTopic(site), "cloud", contract.TypeConfigSet, contract.ConfigSetData{EntryDelayS: 30})
	drivertest.WaitFor(t, "the bad configuration refused", func() bool { return strings.Contains(logs.String(), "refused a message from the cloud") })

	// A photo of the alarm: announced to the cloud, which says where to send it; the
	// agent sends the file over HTTPS, says so, and removes it.
	endpoint := startPhotoEndpoint(t, files)
	jpeg := []byte{0xFF, 0xD8, 'p', 'i', 'c', 0xFF, 0xD9}
	os.MkdirAll(filepath.Join(state, "photos"), 0o700)
	os.WriteFile(filepath.Join(state, "photos", "01JB8ZQ7V2T4X9H6N5R0WYCE00.jpg"), jpeg, 0o600)
	taken := hub.publish(contract.PhotoTakenTopic(site), "video", contract.TypePhotoTaken, contract.PhotoTakenData{
		Photo: "01JB8ZQ7V2T4X9H6N5R0WYCE00", Incident: "inc1", Camera: "cam_caisse", Bytes: int64(len(jpeg)), Width: 640,
	})
	drivertest.WaitFor(t, "photo.taken on the cloud", func() bool { return cloud.has(contract.TypePhotoTaken, taken) })
	cloud.publish(contract.PhotoCmdTopic(site), "cloud", contract.TypePhotoUpload, contract.PhotoUploadData{
		Photo: "01JB8ZQ7V2T4X9H6N5R0WYCE00", URL: endpoint.url + "/v1/photos/01JB8ZQ7V2T4X9H6N5R0WYCE00",
		Token: "tok-photo-secret", Exp: contract.TS(time.Now().Add(10 * time.Minute)),
	})
	drivertest.WaitFor(t, "photo.sent on the cloud", func() bool { return len(cloud.of(contract.TypePhotoSent)) == 1 })
	if got := endpoint.received(); !bytes.Equal(got, jpeg) {
		t.Fatalf("the endpoint got %q", got)
	}
	if _, err := os.Stat(filepath.Join(state, "photos", "01JB8ZQ7V2T4X9H6N5R0WYCE00.jpg")); !os.IsNotExist(err) {
		t.Fatal("a sent photo is removed")
	}
	if hub.of(contract.TypePhotoUpload) != nil || strings.Contains(logs.String(), "tok-photo-secret") {
		t.Fatal("the upload's token reached the hub or the logs")
	}

	// The internet goes down: the hub hears it, and an alarm raised meanwhile waits on
	// the disk until the cloud is back.
	link.cut()
	drivertest.WaitFor(t, "the link down on the hub", func() bool { return hub.lastStatus().Uplink == contract.UplinkDown })
	second := hub.publish(contract.AlarmEventTopic(site), "engine.alarm", contract.TypeAlarmTriggered, triggered)
	drivertest.WaitFor(t, "the alarm queued", func() bool { return hub.lastStatus().Queued > 0 || strings.Contains(logs.String(), "queued=1") })
	waiting := filepath.Join(state, queueName, second+".json") // a file of its own, not the whole queue
	drivertest.WaitFor(t, "the alarm on the disk", func() bool { _, err := os.Stat(waiting); return err == nil })
	link.restore()
	drivertest.WaitFor(t, "the queued alarm on the cloud", func() bool { return cloud.has(contract.TypeAlarmTriggered, second) })
	drivertest.WaitFor(t, "the alarm off the disk", func() bool { _, err := os.Stat(waiting); return os.IsNotExist(err) })
	drivertest.WaitFor(t, "the link up again", func() bool { return hub.lastStatus().Uplink == contract.UplinkUp })
	drivertest.WaitFor(t, "the site online on the cloud", func() bool { return cloud.lastStatus().Uplink == contract.UplinkUp })

	// The cloud numbers what it got without a hole, whatever stayed on the site, and
	// gets a message again by asking for its number.
	seqs := map[uint64]bool{}
	var firstSeq, last uint64
	for _, typ := range []string{contract.TypeAgentStatus, contract.TypeAlarmTriggered, contract.TypePhotoTaken, contract.TypePhotoSent, contract.TypeConfigApplied} {
		for _, env := range cloud.of(typ) {
			if env.Seq == 0 {
				continue // the Last Will, not numbered
			}
			seqs[env.Seq] = true
			last = max(last, env.Seq)
			if env.ID == first {
				firstSeq = env.Seq
			}
		}
	}
	for n := uint64(1); n <= last; n++ {
		if !seqs[n] {
			t.Fatalf("the cloud got numbers up to %d but not %d: it would ask for it in vain", last, n)
		}
	}
	cloud.publish(contract.AgentCmdTopic(site), "cloud", contract.TypeAgentReplay, contract.ReplayData{From: firstSeq, To: firstSeq})
	drivertest.WaitFor(t, "agent.replayed on the cloud", func() bool { return len(cloud.of(contract.TypeAgentReplayed)) == 1 })
	if d, _ := contract.DecodeData[contract.ReplayedData](cloud.of(contract.TypeAgentReplayed)[0]); d.Resent != 1 || len(d.Missing) != 0 {
		t.Fatalf("replayed %+v, want the first alarm resent", d)
	}
	drivertest.WaitFor(t, "the first alarm again on the cloud", func() bool {
		n := 0
		for _, env := range cloud.of(contract.TypeAlarmTriggered) {
			if env.ID == first && env.Seq == firstSeq {
				n++
			}
		}
		return n == 2
	})

	// A clean stop tells the cloud the site is offline, and removes the pid file.
	stop()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not stop")
	}
	drivertest.WaitFor(t, "offline on the cloud", func() bool { return cloud.lastStatus().Uplink == contract.UplinkDown })
	if _, err := os.Stat(filepath.Join(state, pidName)); !os.IsNotExist(err) {
		t.Fatal("the pid file must be removed")
	}

	// What the agent wrote follows the contract.
	for _, s := range []*side{hub, cloud} {
		for _, env := range s.of(contract.TypeAgentStatus) {
			if env.Src != contract.AgentLink {
				t.Errorf("agent.status from %q", env.Src)
			}
			if err := schematest.Check("agent-status.json", env.Data); err != nil {
				t.Errorf("%v", err)
			}
		}
	}
	if strings.Contains(logs.String(), "pw-of-the-site") {
		t.Fatal("the cloud's password shows in the logs")
	}
}

// photoEndpoint plays the cloud's photo upload over HTTPS, with the same authority as
// the cloud's broker.
type photoEndpoint struct {
	url  string
	mu   sync.Mutex
	body []byte
}

func (e *photoEndpoint) received() []byte { e.mu.Lock(); defer e.mu.Unlock(); return e.body }

func startPhotoEndpoint(t *testing.T, files mqtttest.TLSFiles) *photoEndpoint {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(files.Cert, files.Key)
	if err != nil {
		t.Fatal(err)
	}
	e := &photoEndpoint{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("Authorization") != "Bearer tok-photo-secret" {
			http.Error(w, "", http.StatusForbidden)
			return
		}
		b, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.body = b
		e.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	e.url = strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) // the certificate is for localhost
	return e
}
