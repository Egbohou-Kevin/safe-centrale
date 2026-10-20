package agentapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/agent"
	"safehome/internal/contract"
	"safehome/internal/outbox"
	"safehome/internal/schematest"
)

const site = "tg-lom-0001"

var t0 = time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)

// --- fakes ---------------------------------------------------------------------

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*fakeTimer]bool
}

type fakeTimer struct {
	when time.Time
	c    chan time.Time
}

func newClock() *fakeClock { return &fakeClock{now: t0, timers: map[*fakeTimer]bool{}} }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{when: c.now.Add(d), c: make(chan time.Time, 1)}
	if d <= 0 {
		t.c <- c.now
		return t.c, func() {}
	}
	c.timers[t] = true
	return t.c, func() { c.mu.Lock(); delete(c.timers, t); c.mu.Unlock() }
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for t := range c.timers {
		if !t.when.After(c.now) {
			t.c <- c.now
			delete(c.timers, t)
		}
	}
}

type memStore struct {
	mu   sync.Mutex
	docs map[string][]byte
}

func (s *memStore) Save(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[name] = append([]byte(nil), data...)
	return nil
}

func (s *memStore) Load(name string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.docs[name]
	return d, ok, nil
}

type published struct {
	topic  string
	retain bool
	raw    []byte
	env    contract.Envelope
}

// fakeBroker records what is published; while down it answers ErrUnavailable.
type fakeBroker struct {
	mu   sync.Mutex
	msgs []published
	down bool
}

func (b *fakeBroker) Publish(_ context.Context, topic string, payload []byte, _ byte, retain bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return outbox.ErrUnavailable
	}
	env, _ := contract.ParseEnvelope(payload)
	b.msgs = append(b.msgs, published{topic: topic, retain: retain, raw: append([]byte(nil), payload...), env: env})
	return nil
}

func (b *fakeBroker) setDown(down bool) { b.mu.Lock(); b.down = down; b.mu.Unlock() }

func (b *fakeBroker) all() []published {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]published(nil), b.msgs...)
}

func (b *fakeBroker) ofType(typ string) []published {
	var out []published
	for _, m := range b.all() {
		if m.env.Type == typ {
			out = append(out, m)
		}
	}
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- rig -----------------------------------------------------------------------

// memPhotos is the photos' folder.
type memPhotos struct {
	mu      sync.Mutex
	files   map[string][]byte
	removed []string
}

func (m *memPhotos) Open(id string) (io.ReadCloser, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[id]
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s", ErrNoPhoto, id)
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (m *memPhotos) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, id)
	m.removed = append(m.removed, id)
	return nil
}

// fakeUploader plays the cloud's upload endpoint: errs are answered in turn.
type fakeUploader struct {
	mu   sync.Mutex
	errs []error
	got  []string // url token bytes
}

func (u *fakeUploader) Put(_ context.Context, url, token string, body io.Reader, size int64) error {
	b, _ := io.ReadAll(body)
	u.mu.Lock()
	defer u.mu.Unlock()
	if int64(len(b)) != size {
		return fmt.Errorf("size %d, read %d", size, len(b))
	}
	u.got = append(u.got, fmt.Sprintf("%s %s %d", url, token, len(b)))
	if len(u.errs) > 0 {
		err := u.errs[0]
		u.errs = u.errs[1:]
		return err
	}
	return nil
}

func (u *fakeUploader) calls() []string { u.mu.Lock(); defer u.mu.Unlock(); return slices.Clone(u.got) }

type rig struct {
	t      *testing.T
	clock  *fakeClock
	store  *memStore
	local  *fakeBroker
	cloud  *fakeBroker
	photos *memPhotos
	upl    *fakeUploader
	app    *App
	ids    *contract.IDGenerator
	stop   func()
}

func newRig(t *testing.T, store *memStore) *rig {
	t.Helper()
	return newRigWith(t, store, DefaultConfig(site))
}

func newRigWith(t *testing.T, store *memStore, cfg Config) *rig {
	t.Helper()
	if store == nil {
		store = &memStore{docs: map[string][]byte{}}
	}
	r := &rig{t: t, clock: newClock(), store: store, local: &fakeBroker{}, cloud: &fakeBroker{}, ids: contract.NewIDGenerator(),
		photos: &memPhotos{files: map[string][]byte{}}, upl: &fakeUploader{}}
	app, err := New(cfg, Deps{Clock: r.clock, Store: store, Local: r.local, Cloud: r.cloud,
		Photos: r.photos, Uploader: r.upl, NewID: r.ids.New})
	if err != nil {
		t.Fatal(err)
	}
	r.app = app
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { app.Run(ctx); close(done) }()
	r.stop = func() { cancel(); <-done }
	t.Cleanup(r.stop)
	eventually(t, "the first status", func() bool { return len(r.local.ofType(contract.TypeAgentStatus)) > 0 })
	return r
}

// msg builds a message of the site.
func (r *rig) msg(typ string, data any) []byte {
	r.t.Helper()
	env, err := contract.NewEnvelope(site, "hub", 1, r.ids.New(), r.clock.Now(), typ, data)
	if err != nil {
		r.t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	return b
}

func (r *rig) fromHub(topic, typ string, data any) []byte {
	b := r.msg(typ, data)
	r.app.HandleLocal(Message{Topic: topic, Payload: b})
	return b
}

func (r *rig) fromCloud(topic, typ string, data any) []byte {
	b := r.msg(typ, data)
	r.app.HandleCloud(Message{Topic: topic, Payload: b})
	return b
}

func lastStatus(t *testing.T, b *fakeBroker) contract.AgentStatusData {
	t.Helper()
	st := b.ofType(contract.TypeAgentStatus)
	if len(st) == 0 {
		t.Fatal("no agent.status")
	}
	d, err := contract.DecodeData[contract.AgentStatusData](st[len(st)-1].env)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func topic(parts ...string) string { return contract.Topic(site, parts...) }

// same says whether m is the message sent: the agent numbers again what goes to the
// cloud, so only seq may differ.
func same(m published, sent []byte) bool {
	env, err := contract.ParseEnvelope(sent)
	if err != nil {
		return false
	}
	got := m.env
	got.Seq, env.Seq = 0, 0
	return reflect.DeepEqual(got, env)
}

var triggered = contract.TriggeredData{Incident: "inc1", DeviceID: "dev_door", Capability: contract.CapContact, Zone: "Entree"}

// --- tests -----------------------------------------------------------------------

func TestTheStatusSaysWhetherTheClientsDataIsSpared(t *testing.T) {
	if lastStatus(t, newRig(t, nil).local).Economy {
		t.Error("economy on a fixed line")
	}
	cfg := DefaultConfig(site)
	cfg.Relay.Economy = agent.EconomyEvery
	if !lastStatus(t, newRigWith(t, nil, cfg).local).Economy {
		t.Error("economy not said on mobile data: the cloud would take the site for silent")
	}
}

func TestNewNeedsItsPortsAndAGoodConfig(t *testing.T) {
	store := &memStore{docs: map[string][]byte{}}
	good := Deps{Clock: newClock(), Store: store, Local: &fakeBroker{}, Cloud: &fakeBroker{}, Photos: &memPhotos{},
		Uploader: &fakeUploader{}, NewID: contract.NewIDGenerator().New}
	if _, err := New(DefaultConfig(site), Deps{Clock: good.Clock, Store: store, Local: good.Local, NewID: good.NewID}); err == nil {
		t.Error("an agent without the cloud was built")
	}
	bad := DefaultConfig(site)
	bad.StatusEvery = 0
	if _, err := New(bad, good); err == nil {
		t.Error("a zero status period was accepted")
	}
	other := DefaultConfig(site)
	other.Relay.Site = "tg-lom-0002"
	if _, err := New(other, good); err == nil {
		t.Error("two sites were accepted")
	}
	broken := DefaultConfig(site)
	broken.Relay.ConfirmWait = 0
	if _, err := New(broken, good); err == nil {
		t.Error("a broken relay config was accepted")
	}
}

func TestSubscriptionsNeverHearThemselves(t *testing.T) {
	r := newRig(t, nil)
	var local, cloud []string
	for _, s := range r.app.LocalSubscriptions() {
		local = append(local, s.Filter)
		if !s.NoLocal || s.QoS != 1 {
			t.Errorf("%+v", s)
		}
	}
	for _, s := range r.app.CloudSubscriptions() {
		cloud = append(cloud, s.Filter)
		if !s.NoLocal || s.QoS != 1 {
			t.Errorf("%+v", s)
		}
	}
	if !strings.Contains(strings.Join(local, " "), topic("alarm", "event")) || !strings.Contains(strings.Join(cloud, " "), topic("alarm", "cmd")) {
		t.Fatalf("local %v, cloud %v", local, cloud)
	}
}

func TestAtStartTheLinkIsDownAndNothingIsSent(t *testing.T) {
	r := newRig(t, nil)
	st := r.local.ofType(contract.TypeAgentStatus)
	if !st[0].retain || lastStatus(t, r.local).Uplink != contract.UplinkDown {
		t.Fatalf("%+v", st[0])
	}
	if err := schematest.Check("agent-status.json", st[0].env.Data); err != nil {
		t.Fatal(err)
	}
	if err := schematest.Check("envelope.json", st[0].raw); err != nil {
		t.Fatal(err)
	}
	if st[0].env.Src != contract.AgentLink {
		t.Fatalf("src %q", st[0].env.Src)
	}
	r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	time.Sleep(50 * time.Millisecond)
	if n := len(r.cloud.all()); n != 0 {
		t.Fatalf("%d messages went to a cloud the agent is not connected to", n)
	}
}

func TestWhenTheLinkComesTheQueueLeavesAlarmsFirst(t *testing.T) {
	r := newRig(t, nil)
	r.fromHub(topic("system", "health"), contract.TypeHealth, contract.HealthData{Links: map[string]string{}})
	alarm := r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	r.fromHub(topic("device", "dev_door", "event"), contract.TypeDeviceEvent,
		contract.DeviceEventData{DeviceID: "dev_door", Capability: contract.CapContact, Value: json.RawMessage(`"open"`)}) // stays
	time.Sleep(20 * time.Millisecond)

	r.app.LinkChanged(true, "mqtts")
	eventually(t, "the queue to leave", func() bool { return len(r.cloud.all()) >= 3 })
	msgs := r.cloud.all()
	if !same(msgs[0], alarm) || msgs[0].retain {
		t.Fatalf("first to the cloud: %s %s", msgs[0].topic, msgs[0].raw)
	}
	var topics []string
	for _, m := range msgs {
		topics = append(topics, m.topic)
		if m.topic == topic("device", "dev_door", "event") {
			t.Fatal("a raw contact event went to the cloud")
		}
	}
	joined := strings.Join(topics, " ")
	if !strings.Contains(joined, topic("system", "health")) || !strings.Contains(joined, topic("agent", "status")) {
		t.Fatalf("the states should follow: %v", topics)
	}
	if st := lastStatus(t, r.local); st.Uplink != contract.UplinkUp || st.Transport != "mqtts" {
		t.Fatalf("%+v", st)
	}
	eventually(t, "an empty queue", func() bool {
		queued := -1
		r.app.loop.Do(context.Background(), func(now time.Time) []agent.Action { // read the core on its loop
			queued = r.app.core.Status(now).Queued
			return nil
		})
		return queued == 0
	})
}

func TestAMessageTheCloudDidNotTakeIsTriedAgain(t *testing.T) {
	r := newRig(t, nil)
	r.cloud.setDown(true)
	r.app.LinkChanged(true, "wss")
	alarm := r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	time.Sleep(50 * time.Millisecond)
	if len(r.cloud.all()) != 0 {
		t.Fatal("sent to a refusing broker")
	}
	r.cloud.setDown(false)
	r.clock.Advance(time.Second) // the first retry delay
	eventually(t, "the retry", func() bool {
		for _, m := range r.cloud.all() {
			if same(m, alarm) {
				return true
			}
		}
		return false
	})
}

func TestTheCloudGetsAgainWhatItMissed(t *testing.T) {
	r := newRig(t, nil)
	r.app.LinkChanged(true, "mqtts")
	alarm := r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	eventually(t, "the alarm to leave", func() bool { return len(r.cloud.ofType(contract.TypeAlarmTriggered)) == 1 })
	first := r.cloud.ofType(contract.TypeAlarmTriggered)[0]
	if first.env.Seq == 0 {
		t.Fatal("what goes to the cloud is numbered")
	}

	r.fromCloud(topic("agent", "cmd"), contract.TypeAgentReplay, contract.ReplayData{From: first.env.Seq, To: first.env.Seq + 500})
	eventually(t, "the replay", func() bool { return len(r.cloud.ofType(contract.TypeAlarmTriggered)) == 2 })
	again := r.cloud.ofType(contract.TypeAlarmTriggered)[1]
	if !same(again, alarm) || !bytes.Equal(again.raw, first.raw) {
		t.Fatalf("replayed %s, first %s", again.raw, first.raw)
	}
	eventually(t, "the report", func() bool { return len(r.cloud.ofType(contract.TypeAgentReplayed)) == 1 })
	rep, err := contract.DecodeData[contract.ReplayedData](r.cloud.ofType(contract.TypeAgentReplayed)[0].env)
	if err != nil || rep.Resent < 1 || len(rep.Missing) != 1 || rep.Missing[0].Last != first.env.Seq+500 {
		t.Fatalf("%+v %v", rep, err)
	}
	if len(r.local.ofType(contract.TypeAgentReplay)) != 0 {
		t.Fatal("the request is for the agent: the hub never sees it")
	}
	schematest.Validate(t, "agent-replayed.json", rep)
}

func TestAnAppAlertWithoutTheCloudFailsOnTheHub(t *testing.T) {
	r := newRig(t, nil)
	req := r.fromHub(topic("alert", "request"), contract.TypeAlertRequest, contract.AlertRequestData{Incident: "inc1",
		Kind: contract.AlertIntrusion, Urgency: contract.UrgencyCritical, Wave: 1, Contact: "u_owner", Channel: contract.ChannelApp, Text: "ALARME"})
	env, _ := contract.ParseEnvelope(req)
	eventually(t, "the failure", func() bool { return len(r.local.ofType(contract.TypeAlertResult)) == 1 })
	res := r.local.ofType(contract.TypeAlertResult)[0]
	d, _ := contract.DecodeData[contract.AlertResultData](res.env)
	if d.Ref != env.ID || d.Status != contract.ResultFailed || d.Reason != contract.ReasonNoUplink || res.topic != topic("alert", "result") {
		t.Fatalf("%+v %+v", res, d)
	}
	if err := schematest.Check("alert-result.json", res.env.Data); err != nil {
		t.Fatal(err)
	}
}

func TestOrdersFromTheCloudReachTheHubUnchanged(t *testing.T) {
	r := newRig(t, nil)
	order := r.fromCloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp, User: "u_owner"})
	r.fromCloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, contract.AlarmCommandData{Command: contract.CmdDisarm, Origin: contract.OriginKeypad})
	r.fromCloud(topic("device", "dev_siren1", "cmd"), contract.TypeCommand, map[string]string{"command": "siren.on"})
	eventually(t, "the order", func() bool {
		for _, m := range r.local.all() {
			if m.topic == topic("alarm", "cmd") {
				return true
			}
		}
		return false
	})
	time.Sleep(50 * time.Millisecond)
	var orders []published
	for _, m := range r.local.all() {
		if m.topic == topic("alarm", "cmd") || strings.HasSuffix(m.topic, "/cmd") {
			orders = append(orders, m)
		}
	}
	if len(orders) != 1 || !bytes.Equal(orders[0].raw, order) {
		t.Fatalf("%d orders reached the hub: %+v", len(orders), orders)
	}
}

func TestTheQueueSurvivesARestartOfTheAgent(t *testing.T) {
	store := &memStore{docs: map[string][]byte{}}
	r := newRig(t, store)
	alarm := r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	eventually(t, "the queue to be saved", func() bool {
		d, _, _ := store.Load("agent")
		return bytes.Contains(d, []byte("alarm/event"))
	})
	r.stop()

	again := newRig(t, store)
	again.app.LinkChanged(true, "mqtts")
	eventually(t, "the saved message", func() bool {
		for _, m := range again.cloud.all() {
			if same(m, alarm) {
				return true
			}
		}
		return false
	})
	// The agent's own numbering goes on after the restart.
	first := r.local.ofType(contract.TypeAgentStatus)
	second := again.local.ofType(contract.TypeAgentStatus)
	if second[0].env.Seq <= first[len(first)-1].env.Seq {
		t.Fatalf("seq went from %d back to %d", first[len(first)-1].env.Seq, second[0].env.Seq)
	}
}

func TestTheStatusIsRepeated(t *testing.T) {
	r := newRig(t, nil)
	n := len(r.local.ofType(contract.TypeAgentStatus))
	r.clock.Advance(time.Minute)
	eventually(t, "the repeated status", func() bool { return len(r.local.ofType(contract.TypeAgentStatus)) > n })
}

func TestTheWillSaysTheSiteIsOffline(t *testing.T) {
	r := newRig(t, nil)
	tp, payload := r.app.Will()
	env, err := contract.ParseEnvelope(payload)
	if err != nil || tp != topic("agent", "status") || env.Seq != 0 {
		t.Fatalf("%s %+v %v", tp, env, err)
	}
	if d, _ := contract.DecodeData[contract.AgentStatusData](env); d.Uplink != contract.UplinkDown {
		t.Fatalf("%+v", d)
	}
	_, again := r.app.Will()
	if bytes.Equal(payload, again) {
		t.Fatal("two wills with the same id: the cloud would drop the second as a duplicate")
	}
}

func TestOversizedMessagesAreLeftAside(t *testing.T) {
	r := newRig(t, nil)
	r.app.LinkChanged(true, "mqtts")
	huge := bytes.Repeat([]byte("x"), maxPayload+1)
	r.app.HandleLocal(Message{Topic: topic("alarm", "event"), Payload: huge})
	r.app.HandleCloud(Message{Topic: topic("alarm", "cmd"), Payload: huge})
	time.Sleep(50 * time.Millisecond)
	for _, m := range append(r.cloud.all(), r.local.all()...) {
		if len(m.raw) > maxPayload {
			t.Fatal("an oversized message went through")
		}
	}
}

func TestRunTwiceIsRefused(t *testing.T) {
	r := newRig(t, nil)
	if err := r.app.Run(context.Background()); err == nil {
		t.Fatal("Run twice")
	}
}

// --- photos ----------------------------------------------------------------------------

func (r *rig) photoTaken(id string, size int) {
	r.photos.mu.Lock()
	r.photos.files[id] = make([]byte, size)
	r.photos.mu.Unlock()
	r.fromHub(topic("photo", "taken"), contract.TypePhotoTaken, contract.PhotoTakenData{Photo: id, Incident: "inc1", Camera: "cam_caisse", Bytes: int64(size), Width: 640})
}

func (r *rig) uploadOrder(id string) {
	r.fromCloud(topic("photo", "cmd"), contract.TypePhotoUpload, contract.PhotoUploadData{
		Photo: id, URL: "https://api.safehome.example/v1/photos/" + id, Token: "tok-" + id, Exp: contract.TS(r.clock.Now().Add(time.Hour)),
	})
}

func (r *rig) sent(id string) []contract.PhotoSentData {
	var out []contract.PhotoSentData
	for _, p := range r.local.ofType(contract.TypePhotoSent) {
		d, _ := contract.DecodeData[contract.PhotoSentData](p.env)
		if d.Photo == id {
			out = append(out, d)
		}
	}
	return out
}

func TestAPhotoIsUploadedThenRemoved(t *testing.T) {
	r := newRig(t, nil)
	r.app.LinkChanged(true, "mqtts")
	r.photoTaken("p1", 4000)
	eventually(t, "photo.taken in the cloud", func() bool { return len(r.cloud.ofType(contract.TypePhotoTaken)) == 1 })
	r.uploadOrder("p1")
	eventually(t, "the upload", func() bool { return len(r.upl.calls()) == 1 })
	if c := r.upl.calls()[0]; c != "https://api.safehome.example/v1/photos/p1 tok-p1 4000" {
		t.Fatalf("%s", c)
	}
	eventually(t, "photo.sent", func() bool { return len(r.sent("p1")) == 1 })
	if d := r.sent("p1")[0]; d.Status != contract.PhotoSent {
		t.Fatalf("%+v", d)
	}
	eventually(t, "photo.sent in the cloud", func() bool { return len(r.cloud.ofType(contract.TypePhotoSent)) == 1 })
	r.photos.mu.Lock()
	defer r.photos.mu.Unlock()
	if len(r.photos.files) != 0 || !slices.Equal(r.photos.removed, []string{"p1"}) {
		t.Fatalf("%v", r.photos.removed)
	}
	for _, m := range r.local.all() {
		if strings.Contains(string(m.raw), "tok-p1") {
			t.Fatal("the token reached the hub")
		}
	}
}

func TestUploadFailures(t *testing.T) {
	r := newRig(t, nil)
	r.app.LinkChanged(true, "mqtts")
	r.upl.errs = []error{errors.New("connection reset"), fmt.Errorf("%w: 403", ErrUploadExpired)}
	r.photoTaken("p1", 100)
	r.uploadOrder("p1")
	eventually(t, "the first try", func() bool { return len(r.upl.calls()) == 1 })
	time.Sleep(20 * time.Millisecond)
	r.clock.Advance(30 * time.Second)
	eventually(t, "the second try", func() bool { return len(r.upl.calls()) == 2 })
	eventually(t, "url_expired", func() bool { return len(r.sent("p1")) == 1 })
	if d := r.sent("p1")[0]; d.Reason != contract.PhotoURLExpired {
		t.Fatalf("%+v", d)
	}
	// A new address, and a file refused.
	r.upl.mu.Lock()
	r.upl.errs = []error{fmt.Errorf("%w: 413", ErrUploadRefused)}
	r.upl.mu.Unlock()
	r.uploadOrder("p1")
	eventually(t, "refused", func() bool { return len(r.sent("p1")) == 2 })
	if d := r.sent("p1")[1]; d.Reason != contract.PhotoRefused {
		t.Fatalf("%+v", d)
	}
	// A photo whose file is gone.
	r.fromHub(topic("photo", "taken"), contract.TypePhotoTaken, contract.PhotoTakenData{Photo: "p2", Incident: "inc1", Camera: "c", Bytes: 1, Width: 1})
	r.uploadOrder("p2")
	eventually(t, "missing", func() bool { return len(r.sent("p2")) == 1 && r.sent("p2")[0].Reason == contract.PhotoMissing })
	if n := len(r.upl.calls()); n != 3 {
		t.Fatalf("%d uploads", n)
	}
}
