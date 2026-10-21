package videoapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/inbox"
	"safehome/internal/schematest"
	"safehome/internal/video"
)

const site = "tg-lom-0001"

var t0 = time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)

// --- fakes ---------------------------------------------------------------------------

type fakeBroker struct {
	mu   sync.Mutex
	msgs []contract.Envelope
	ret  map[string]bool // topic -> retained
}

func (b *fakeBroker) Publish(_ context.Context, topic string, payload []byte, _ byte, retain bool) error {
	env, err := contract.ParseEnvelope(payload)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, env)
	b.ret[topic] = retain
	return nil
}

func (b *fakeBroker) sessions() []contract.VideoSessionData {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []contract.VideoSessionData
	for _, e := range b.msgs {
		if e.Type == contract.TypeVideoSession {
			d, _ := contract.DecodeData[contract.VideoSessionData](e)
			out = append(out, d)
		}
	}
	return out
}

func (b *fakeBroker) status() contract.VideoStatusData {
	b.mu.Lock()
	defer b.mu.Unlock()
	var d contract.VideoStatusData
	for _, e := range b.msgs {
		if e.Type == contract.TypeVideoStatus {
			d, _ = contract.DecodeData[contract.VideoStatusData](e)
		}
	}
	return d
}

func (b *fakeBroker) count(typ string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, e := range b.msgs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func (b *fakeBroker) has(session string, status string, reason string) bool {
	for _, s := range b.sessions() {
		if s.Session == session && s.Status == status && s.Reason == reason {
			return true
		}
	}
	return false
}

type fakeStream struct {
	mu      sync.Mutex
	bytes   int64
	err     error
	stopped bool
}

func (s *fakeStream) Sent(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes, s.err
}

func (s *fakeStream) Stop() { s.mu.Lock(); s.stopped = true; s.mu.Unlock() }

func (s *fakeStream) set(n int64, err error) { s.mu.Lock(); s.bytes, s.err = n, err; s.mu.Unlock() }

func (s *fakeStream) isStopped() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.stopped }

type fakeStreamer struct {
	mu       sync.Mutex
	targets  []string
	streams  map[string]*fakeStream // camera -> its last stream
	startErr map[string]error
	down     map[string]bool // cameras that do not answer a probe
}

func (f *fakeStreamer) Start(_ context.Context, camera, target string) (Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = append(f.targets, target)
	if err := f.startErr[camera]; err != nil {
		return nil, err
	}
	s := &fakeStream{}
	f.streams[camera] = s
	return s, nil
}

func (f *fakeStreamer) Probe(_ context.Context, camera string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[camera] {
		return errors.New("no route to host")
	}
	return nil
}

func (f *fakeStreamer) stream(camera string) *fakeStream {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streams[camera]
}

// fakeCamera takes pictures: frameErr for a camera that gives none.
type fakeCamera struct {
	mu       sync.Mutex
	openErr  error
	frameErr map[string]error
	frames   []string // camera@width, in order
	opened   int
	closed   int
}

func (f *fakeCamera) Open(context.Context) (Shooter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.opened++
	return f, nil
}

func (f *fakeCamera) Frame(_ context.Context, camera string, width int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.frameErr[camera]; err != nil {
		return nil, err
	}
	f.frames = append(f.frames, fmt.Sprintf("%s@%d", camera, width))
	return make([]byte, width*10), nil
}

func (f *fakeCamera) Close() { f.mu.Lock(); f.closed++; f.mu.Unlock() }

func (f *fakeCamera) count() (frames, opened, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.frames), f.opened, f.closed
}

type memPhotos struct {
	mu    sync.Mutex
	saved map[string]int
	err   error
}

func (m *memPhotos) Save(id string, jpeg []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.saved[id] = len(jpeg)
	return nil
}

func (m *memPhotos) size(id string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.saved[id]
	return n, ok
}

// --- harness -------------------------------------------------------------------------

type harness struct {
	t       *testing.T
	app     *App
	broker  *fakeBroker
	stream  *fakeStreamer
	camera  *fakeCamera
	photos  *memPhotos
	logs    *drivertest.SyncBuffer
	clock   *drivertest.FakeClock
	ids     *contract.IDGenerator
	stopRun func()
	done    chan struct{}
}

func start(t *testing.T, edit ...func(*Config)) *harness {
	t.Helper()
	v := video.DefaultConfig([]video.Camera{{ID: "cam_caisse", Name: "Caisse"}, {ID: "cam_cour", Name: "Cour"}})
	v.StartTimeout, v.StallAfter = 2*time.Second, 2*time.Second
	cfg := DefaultConfig(site, v)
	cfg.ProgressEvery, cfg.Outbox.RetryMin, cfg.Outbox.RetryMax = 10*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond
	for _, e := range edit {
		e(&cfg)
	}
	h := &harness{
		t: t, broker: &fakeBroker{ret: map[string]bool{}}, logs: drivertest.Logs(t), clock: drivertest.NewFakeClock(t0), ids: contract.NewIDGenerator(),
		stream: &fakeStreamer{streams: map[string]*fakeStream{}, startErr: map[string]error{}, down: map[string]bool{}},
		camera: &fakeCamera{frameErr: map[string]error{}}, photos: &memPhotos{saved: map[string]int{}},
		done: make(chan struct{}),
	}
	app, err := New(cfg, Deps{
		Clock: h.clock, Store: drivertest.NewMemStore(), Publisher: h.broker,
		Streamer: h.stream, Camera: h.camera, Photos: h.photos, NewID: h.ids.New, Logger: slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.app = app
	ctx, cancel := context.WithCancel(context.Background())
	h.stopRun = cancel
	go func() { app.Run(ctx); close(h.done) }()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	h.stopRun()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Fatal("the service did not stop")
	}
}

func (h *harness) send(typ string, data any) {
	h.t.Helper()
	env, err := contract.NewEnvelope(site, "cloud", 1, h.ids.New(), h.clock.Now(), typ, data)
	if err != nil {
		h.t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	h.app.HandleMessage(inbox.Message{Topic: contract.VideoCmdTopic(site), Payload: b})
}

func (h *harness) request(session, camera string) {
	h.send(contract.TypeVideoRequest, contract.VideoRequestData{
		Session: session, Camera: camera, Reason: contract.VideoOnDemand, User: "u_owner",
		PushURL: "rtmps://relay.safehome.example/live/" + session, Token: "tok-" + session + "-secret",
		Exp: contract.TS(h.clock.Now().Add(30 * time.Second)),
	})
}

// idle waits until the loop has taken what was given to it and the work away from
// it (probes, photos) is over. Not while a stream runs: its work ends with it.
func (h *harness) idle() {
	h.t.Helper()
	h.app.Alive(context.Background())
	done := make(chan struct{})
	go func() { h.app.work.Wait(); close(done) }()
	drivertest.WaitFor(h.t, "the work away from the loop", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	h.app.Alive(context.Background())
}

// progress lets a stream's bytes be read once more.
func (h *harness) progress() { h.t.Helper(); h.clock.PassTimer(h.t, h.app.cfg.ProgressEvery) }

// --- tests ---------------------------------------------------------------------------

func TestASessionFromRequestToStop(t *testing.T) {
	h := start(t)
	h.request("s1", "cam_caisse")
	drivertest.WaitFor(t, "the stream", func() bool { return h.stream.stream("cam_caisse") != nil })
	h.stream.mu.Lock()
	target := h.stream.targets[0]
	h.stream.mu.Unlock()
	if target != "rtmps://relay.safehome.example/live/s1?token=tok-s1-secret" {
		t.Fatalf("%q", target)
	}
	drivertest.WaitFor(t, "starting", func() bool { return h.broker.has("s1", contract.VideoStarting, "") })
	drivertest.WaitFor(t, "one session in the status", func() bool { return h.broker.status().Active == 1 })

	s := h.stream.stream("cam_caisse")
	s.set(250_000, nil)
	h.progress()
	drivertest.WaitFor(t, "live", func() bool { return h.broker.has("s1", contract.VideoLive, "") })
	s.set(3<<20, nil)
	h.progress()
	drivertest.WaitFor(t, "the usage", func() bool { return h.broker.status().UsedMBMonth == 3 })

	h.send(contract.TypeVideoStop, contract.VideoStopData{Session: "s1"})
	drivertest.WaitFor(t, "ended", func() bool { return h.broker.has("s1", contract.VideoEnded, contract.VideoStopped) })
	drivertest.WaitFor(t, "the stream stopped", s.isStopped)
	drivertest.WaitFor(t, "no session in the status", func() bool { return h.broker.status().Active == 0 })

	for _, d := range h.broker.sessions() {
		raw, _ := json.Marshal(d)
		if err := schematest.Check("video-session.json", raw); err != nil {
			t.Error(err)
		}
		if d.Status == contract.VideoEnded && d.Bytes != 3<<20 {
			t.Errorf("%+v", d)
		}
	}
	raw, _ := json.Marshal(h.broker.status())
	if err := schematest.Check("video-status.json", raw); err != nil {
		t.Fatal(err)
	}
	if !h.broker.ret[contract.VideoStatusTopic(site)] || h.broker.ret[contract.VideoSessionTopic(site)] {
		t.Fatal("video.status is retained, video.session is not")
	}
	if strings.Contains(h.logs.String(), "secret") {
		t.Fatalf("the token shows in the logs:\n%s", h.logs.String())
	}
}

func TestTheCameraDoesNotAnswer(t *testing.T) {
	h := start(t)
	h.stream.startErr["cam_cour"] = fmt.Errorf("streams: dial tcp: refused: %w", ErrCameraUnreachable)
	h.request("s1", "cam_cour")
	drivertest.WaitFor(t, "failed", func() bool { return h.broker.has("s1", contract.VideoFailed, contract.VideoCameraUnreachable) })
}

func TestAnyOtherStartErrorIsTheRelays(t *testing.T) {
	h := start(t)
	h.stream.startErr["cam_cour"] = errors.New("go2rtc did not start")
	h.request("s1", "cam_cour")
	drivertest.WaitFor(t, "failed", func() bool { return h.broker.has("s1", contract.VideoFailed, contract.VideoRelayUnreachable) })
}

func TestAStreamThatBreaksEndsTheSession(t *testing.T) {
	h := start(t)
	h.request("s1", "cam_caisse")
	drivertest.WaitFor(t, "the stream", func() bool { return h.stream.stream("cam_caisse") != nil })
	s := h.stream.stream("cam_caisse")
	s.set(1000, nil)
	h.progress()
	drivertest.WaitFor(t, "live", func() bool { return h.broker.has("s1", contract.VideoLive, "") })
	s.set(1000, errors.New("go2rtc exited"))
	h.progress()
	drivertest.WaitFor(t, "ended", func() bool { return h.broker.has("s1", contract.VideoEnded, contract.VideoRelayUnreachable) })
	drivertest.WaitFor(t, "stopped", s.isStopped)
}

func TestNothingReachesTheRelay(t *testing.T) {
	h := start(t)
	h.request("s1", "cam_caisse")
	h.clock.PassTimer(t, 2*time.Second) // the start timeout
	drivertest.WaitFor(t, "failed after the start timeout", func() bool {
		return h.broker.has("s1", contract.VideoFailed, contract.VideoRelayUnreachable)
	})
	drivertest.WaitFor(t, "stopped", h.stream.stream("cam_caisse").isStopped)
}

func TestStoppingTheServiceStopsEveryStream(t *testing.T) {
	h := start(t)
	h.request("s1", "cam_caisse")
	h.request("s2", "cam_cour")
	drivertest.WaitFor(t, "two streams", func() bool { return h.stream.stream("cam_caisse") != nil && h.stream.stream("cam_cour") != nil })
	h.stop()
	if !h.stream.stream("cam_caisse").isStopped() || !h.stream.stream("cam_cour").isStopped() {
		t.Fatal("a stream outlived the service")
	}
}

func TestCamerasAreProbed(t *testing.T) {
	h := start(t)
	h.stream.mu.Lock()
	h.stream.down["cam_cour"] = true
	h.stream.mu.Unlock()
	drivertest.WaitFor(t, "the probe", func() bool {
		st := h.broker.status()
		return len(st.Cameras) == 2 && st.Cameras[0].Reachable
	})
	if st := h.broker.status(); st.Cameras[1].Reachable || st.Uplink != contract.UplinkFixed || st.QuotaMB != 0 {
		t.Fatalf("%+v", st)
	}
}

// video.status is retained and goes to the cloud, on mobile data maybe: sent again
// unchanged every minute, it cost some 20 MB a month for nothing.
func TestTheStatusGoesOutOnChangeOnly(t *testing.T) {
	h := start(t)
	reachable := func(want bool) func() bool {
		return func() bool { st := h.broker.status(); return len(st.Cameras) == 2 && st.Cameras[1].Reachable == want }
	}
	drivertest.WaitFor(t, "the first probe", reachable(true))
	before := h.broker.count(contract.TypeVideoStatus)
	for range 12 { // two probes, the same answers
		h.clock.Advance(time.Minute)
		h.idle() // the probes' answers too
	}
	if n := h.broker.count(contract.TypeVideoStatus); n != before {
		t.Fatalf("%d statuses published without a change", n-before)
	}
	h.stream.mu.Lock()
	h.stream.down["cam_cour"] = true
	h.stream.mu.Unlock()
	h.clock.Advance(5 * time.Minute)
	drivertest.WaitFor(t, "the camera unreachable", reachable(false))
	if n := h.broker.count(contract.TypeVideoStatus); n != before+1 {
		t.Fatalf("%d statuses for one change", n-before)
	}
}

func TestBadOrdersAreIgnored(t *testing.T) {
	h := start(t)
	ok := func(session string) contract.VideoRequestData {
		return contract.VideoRequestData{Session: session, Camera: "cam_caisse", Reason: contract.VideoOnDemand,
			PushURL: "rtmps://relay.example/live/x", Token: "t", Exp: contract.TS(h.clock.Now().Add(time.Minute))}
	}
	bad := ok("s1")
	bad.PushURL = "rtmp://relay.example/live/x" // not encrypted
	h.send(contract.TypeVideoRequest, bad)
	h.send(contract.TypeVideoStop, contract.VideoStopData{})
	h.send(contract.TypeAlarmCommand, contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp})
	env, _ := contract.NewEnvelope("tg-lom-9999", "cloud", 1, h.ids.New(), h.clock.Now(), contract.TypeVideoRequest, ok("s2"))
	b, _ := json.Marshal(env)
	h.app.HandleMessage(inbox.Message{Topic: contract.VideoCmdTopic(site), Payload: b})
	h.app.HandleMessage(inbox.Message{Topic: contract.VideoCmdTopic(site), Payload: []byte("{")})
	h.app.HandleMessage(inbox.Message{Topic: contract.AlarmCmdTopic(site), Payload: b})

	// A duplicate is ignored too.
	env, _ = contract.NewEnvelope(site, "cloud", 1, h.ids.New(), h.clock.Now(), contract.TypeVideoRequest, ok("s3"))
	b, _ = json.Marshal(env)
	h.app.HandleMessage(inbox.Message{Topic: contract.VideoCmdTopic(site), Payload: b})
	h.app.HandleMessage(inbox.Message{Topic: contract.VideoCmdTopic(site), Payload: b})
	drivertest.WaitFor(t, "s3 starting", func() bool { return h.broker.has("s3", contract.VideoStarting, "") })
	drivertest.WaitFor(t, "s3's stream", func() bool { return h.stream.stream("cam_caisse") != nil })
	h.app.Alive(context.Background()) // every order taken
	drivertest.WaitFor(t, "the outbox to empty", func() bool { return h.app.out.Depth() == 0 })
	h.stream.mu.Lock()
	n := len(h.stream.targets)
	h.stream.mu.Unlock()
	if n != 1 || len(h.broker.sessions()) != 1 {
		t.Fatalf("only s3 starts, once: %d %+v", n, h.broker.sessions())
	}
}

func TestConfigValidation(t *testing.T) {
	v := video.DefaultConfig([]video.Camera{{ID: "cam"}})
	if err := DefaultConfig(site, v).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*Config){
		func(c *Config) { c.Site = "X" },
		func(c *Config) { c.ProgressEvery = 0 },
		func(c *Config) { c.Video.Cameras = nil },
	} {
		c := DefaultConfig(site, v)
		edit(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	if _, err := New(DefaultConfig(site, v), Deps{}); err == nil {
		t.Fatal("missing ports accepted")
	}
}

// --- photos ----------------------------------------------------------------------------

func (h *harness) alarm(incident string) {
	h.t.Helper()
	env, _ := contract.NewEnvelope(site, "engine", 1, h.ids.New(), h.clock.Now(), contract.TypeAlarmTriggered,
		contract.TriggeredData{Incident: incident, DeviceID: "dev_door", Capability: contract.CapContact, Zone: "entrance"})
	b, _ := json.Marshal(env)
	h.app.HandleMessage(inbox.Message{Topic: contract.AlarmEventTopic(site), Payload: b})
	h.app.HandleMessage(inbox.Message{Topic: contract.AlarmEventTopic(site), Payload: b}) // QoS 1: again
}

func (b *fakeBroker) photos() []contract.PhotoTakenData {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []contract.PhotoTakenData
	for _, e := range b.msgs {
		if e.Type == contract.TypePhotoTaken {
			d, _ := contract.DecodeData[contract.PhotoTakenData](e)
			out = append(out, d)
		}
	}
	return out
}

func quickPhotos(c *Config) {
	c.Video.Photos = video.PhotoPolicy{Count: 2, Every: 500 * time.Millisecond, Width: 320, PerHour: 6}
}

func TestAnAlarmIsPhotographedAndEachPhotoAnnounced(t *testing.T) {
	h := start(t, quickPhotos)
	h.alarm("inc1")
	h.clock.PassTimer(t, 500*time.Millisecond) // between the two rounds
	h.idle()
	if frames, opened, _ := h.camera.count(); frames != 4 || opened != 1 {
		t.Fatalf("%d pictures, camera opened %d times: one set per alarm", frames, opened)
	}
	drivertest.WaitFor(t, "the outbox to empty", func() bool { return h.app.out.Depth() == 0 })
	if n := len(h.broker.photos()); n != 4 {
		t.Fatalf("%d photos announced", n)
	}
	for _, p := range h.broker.photos() {
		raw, _ := json.Marshal(p)
		if err := schematest.Check("photo-taken.json", raw); err != nil {
			t.Fatal(err)
		}
		if n, ok := h.photos.size(p.Photo); !ok || int64(n) != p.Bytes || p.Incident != "inc1" || p.Width != 320 {
			t.Fatalf("%+v kept %d %v", p, n, ok)
		}
	}
	h.camera.mu.Lock()
	order := strings.Join(h.camera.frames, " ")
	h.camera.mu.Unlock()
	if order != "cam_caisse@320 cam_cour@320 cam_caisse@320 cam_cour@320" {
		t.Fatalf("%s", order)
	}
	drivertest.WaitFor(t, "the camera freed", func() bool { _, o, c := h.camera.count(); return o == 1 && c == 1 })
	h.broker.mu.Lock()
	retained := h.broker.ret[contract.PhotoTakenTopic(site)]
	h.broker.mu.Unlock()
	if retained {
		t.Fatal("photo.taken is not retained")
	}
}

func TestACameraWithoutPictureIsLeftOut(t *testing.T) {
	h := start(t, quickPhotos)
	h.camera.frameErr["cam_cour"] = fmt.Errorf("%w: no picture", ErrCameraUnreachable)
	h.alarm("inc1")
	h.clock.PassTimer(t, 500*time.Millisecond) // between the two rounds
	drivertest.WaitFor(t, "two photos", func() bool { return len(h.broker.photos()) == 2 })
	drivertest.WaitFor(t, "the end", func() bool { return strings.Contains(h.logs.String(), "photos of the alarm taken") })
	if f, _, _ := h.camera.count(); f != 2 || strings.Count(h.logs.String(), "a camera gave no photo") != 1 {
		t.Fatalf("%d frames\n%s", f, h.logs.String())
	}
}

func TestPhotosThatCannotBeTakenOrKept(t *testing.T) {
	h := start(t, quickPhotos)
	h.camera.openErr = errors.New("go2rtc: did not start")
	h.alarm("inc1")
	drivertest.WaitFor(t, "the failure said", func() bool { return strings.Contains(h.logs.String(), "could not be taken") })

	h.camera.mu.Lock()
	h.camera.openErr = nil
	h.camera.mu.Unlock()
	h.photos.mu.Lock()
	h.photos.err = errors.New("disk full")
	h.photos.mu.Unlock()
	h.alarm("inc2")
	h.clock.PassTimer(t, 500*time.Millisecond) // between the two rounds
	drivertest.WaitFor(t, "the failure said", func() bool { return strings.Count(h.logs.String(), "could not be kept") == 4 })
	if len(h.broker.photos()) != 0 {
		t.Fatal("a photo not kept is not announced")
	}
	// Other alarm events, and garbage, take no photo.
	env, _ := contract.NewEnvelope(site, "engine", 1, h.ids.New(), h.clock.Now(), contract.TypeAlarmArmed, contract.ArmedData{})
	b, _ := json.Marshal(env)
	h.app.HandleMessage(inbox.Message{Topic: contract.AlarmEventTopic(site), Payload: b})
	h.app.HandleMessage(inbox.Message{Topic: contract.AlarmEventTopic(site), Payload: []byte("{")})
	if _, o, _ := h.camera.count(); o != 1 {
		t.Fatalf("%d opened", o)
	}
}

func TestTooManyAlarmsAnHour(t *testing.T) {
	h := start(t, func(c *Config) { quickPhotos(c); c.Video.Photos.PerHour, c.Video.Photos.Count = 1, 1 })
	h.alarm("inc1")
	h.alarm("inc2")
	drivertest.WaitFor(t, "the limit said", func() bool { return strings.Contains(h.logs.String(), "no photos for this alarm") })
}
