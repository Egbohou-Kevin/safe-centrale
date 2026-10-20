package maintapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/maint"
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

type broker struct {
	mu   sync.Mutex
	msgs []contract.MaintSessionData
}

func (b *broker) Publish(_ context.Context, topic string, payload []byte, _ byte, retain bool) error {
	env, err := contract.ParseEnvelope(payload)
	if err != nil || topic != contract.MaintSessionTopic(site) || env.Type != contract.TypeMaintSession || retain {
		return errors.New("unexpected " + topic)
	}
	if err := schematest.Check("maint-session.json", env.Data); err != nil {
		return err
	}
	d, _ := contract.DecodeData[contract.MaintSessionData](env)
	b.mu.Lock()
	b.msgs = append(b.msgs, d)
	b.mu.Unlock()
	return nil
}

func (b *broker) steps() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, d := range b.msgs {
		out = append(out, strings.TrimSuffix(d.Status+"/"+d.Reason, "/"))
	}
	return out
}

type fakeTunnel struct {
	up, done chan struct{}
	once     sync.Once
	stopped  bool
	mu       sync.Mutex
}

func (t *fakeTunnel) Up() <-chan struct{}   { return t.up }
func (t *fakeTunnel) Done() <-chan struct{} { return t.done }
func (t *fakeTunnel) Stop() {
	t.mu.Lock()
	t.stopped = true
	t.mu.Unlock()
	t.once.Do(func() { close(t.done) })
}
func (t *fakeTunnel) isStopped() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.stopped }

// breaks says the tunnel stopped by itself.
func (t *fakeTunnel) breaks() { t.once.Do(func() { close(t.done) }) }

type fakeTunneler struct {
	mu      sync.Mutex
	err     error
	tunnels []*fakeTunnel
}

func (f *fakeTunneler) Open(context.Context) (Tunnel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	t := &fakeTunnel{up: make(chan struct{}), done: make(chan struct{})}
	f.tunnels = append(f.tunnels, t)
	return t, nil
}

func (f *fakeTunneler) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.tunnels) }

func (f *fakeTunneler) last() *fakeTunnel {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tunnels[len(f.tunnels)-1]
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

// --- harness -------------------------------------------------------------------

type rig struct {
	t      *testing.T
	app    *App
	clock  *fakeClock
	store  *memStore
	broker *broker
	tun    *fakeTunneler
	logs   *syncBuffer
	ids    *contract.IDGenerator
	stop   func()
}

func start(t *testing.T, store *memStore, clock *fakeClock, edit ...func(*maint.Config)) *rig {
	t.Helper()
	if store == nil {
		store = &memStore{docs: map[string][]byte{}}
	}
	if clock == nil {
		clock = &fakeClock{now: t0, timers: map[*fakeTimer]bool{}}
	}
	m := maint.DefaultConfig()
	for _, e := range edit {
		e(&m)
	}
	cfg := DefaultConfig(site, m)
	cfg.RetryMin, cfg.RetryMax = 5*time.Millisecond, 20*time.Millisecond
	r := &rig{t: t, clock: clock, store: store, broker: &broker{}, tun: &fakeTunneler{}, logs: &syncBuffer{}, ids: contract.NewIDGenerator()}
	app, err := New(cfg, Deps{Clock: clock, Store: store, Publisher: r.broker, Tunneler: r.tun, NewID: r.ids.New,
		Logger: slog.New(slog.NewTextHandler(r.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	r.app = app
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { app.Run(ctx); close(done) }()
	var once sync.Once
	r.stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(r.stop)
	return r
}

func (r *rig) send(typ string, data any) {
	r.t.Helper()
	env, err := contract.NewEnvelope(site, "cloud", 1, r.ids.New(), r.clock.Now(), typ, data)
	if err != nil {
		r.t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	r.app.HandleMessage(Message{Topic: contract.MaintCmdTopic(site), Payload: b})
	r.app.HandleMessage(Message{Topic: contract.MaintCmdTopic(site), Payload: b}) // QoS 1: again
}

func (r *rig) open(session string) {
	r.send(contract.TypeMaintOpen, contract.MaintOpenData{Session: session, User: "kevin", Reason: "modem", DurationS: 3600,
		Exp: contract.TS(r.clock.Now().Add(time.Minute))})
}

func (r *rig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s\nsteps: %v\nlogs:\n%s", what, r.broker.steps(), r.logs.String())
		}
	}
}

func (r *rig) steps(want ...string) {
	r.t.Helper()
	r.waitFor(strings.Join(want, " "), func() bool { return strings.Join(r.broker.steps(), " ") == strings.Join(want, " ") })
}

// settle lets the loop take what was given to it.
func (r *rig) settle() { r.app.Alive(context.Background()) }

// --- tests ---------------------------------------------------------------------

func TestASessionFromOpenToClose(t *testing.T) {
	r := start(t, nil, nil)
	r.open("m1")
	r.waitFor("the tunnel", func() bool { return r.tun.count() == 1 })
	r.steps("opening")
	close(r.tun.last().up)
	r.steps("opening", "open")
	r.send(contract.TypeMaintClose, contract.MaintCloseData{Session: "m1"})
	r.steps("opening", "open", "closed/stopped")
	r.waitFor("the tunnel closed", r.tun.last().isStopped)
	if !strings.Contains(r.logs.String(), "user=kevin") || !strings.Contains(r.logs.String(), "reason=modem") {
		t.Fatalf("who and why are logged:\n%s", r.logs.String())
	}
	if r.tun.count() != 1 {
		t.Fatal("opened once")
	}
}

func TestTheSessionEndsWithItsTime(t *testing.T) {
	r := start(t, nil, nil)
	r.open("m1")
	r.waitFor("the tunnel", func() bool { return r.tun.count() == 1 })
	close(r.tun.last().up)
	r.steps("opening", "open")
	r.clock.Advance(time.Hour)
	r.steps("opening", "open", "closed/timeout")
	r.waitFor("the tunnel closed", r.tun.last().isStopped)
}

func TestATunnelThatBreaksIsOpenedAgain(t *testing.T) {
	r := start(t, nil, nil)
	r.open("m1")
	r.waitFor("the tunnel", func() bool { return r.tun.count() == 1 })
	close(r.tun.last().up)
	r.steps("opening", "open")
	r.tun.last().breaks()
	r.waitFor("the break said", func() bool { return strings.Contains(r.logs.String(), "tunnel stopped") })
	r.settle()
	r.clock.Advance(10 * time.Second)
	r.waitFor("the second tunnel", func() bool { return r.tun.count() == 2 })
	close(r.tun.last().up)
	r.settle()
	r.steps("opening", "open")
}

func TestABastionThatRefuses(t *testing.T) {
	r := start(t, nil, nil)
	r.tun.err = errors.New("ssh: not found")
	r.open("m1")
	r.steps("opening", "failed/unreachable")
	r.tun.mu.Lock()
	r.tun.err = nil
	r.tun.mu.Unlock()
	r.open("m2")
	r.waitFor("the tunnel", func() bool { return r.tun.count() == 1 })
	r.tun.last().breaks() // never up
	r.steps("opening", "failed/unreachable", "opening", "failed/unreachable")
}

func TestRefusedOrders(t *testing.T) {
	r := start(t, nil, nil, func(c *maint.Config) { c.Enabled = false })
	r.open("m1")
	r.steps("failed/disabled")
	r.send(contract.TypeMaintOpen, contract.MaintOpenData{Session: "m2", User: "kevin", DurationS: 600}) // invalid: ignored
	r.send(contract.TypeMaintClose, contract.MaintCloseData{})
	r.app.HandleMessage(Message{Topic: contract.MaintCmdTopic(site), Payload: []byte("{")})
	r.app.HandleMessage(Message{Topic: "elsewhere", Payload: []byte("{}")})
	r.settle()
	time.Sleep(20 * time.Millisecond)
	if s := r.broker.steps(); len(s) != 1 {
		t.Fatalf("%v", s)
	}
	if _, err := New(Config{Site: "X"}, Deps{}); err == nil {
		t.Fatal("bad site")
	}
	if _, err := New(DefaultConfig(site, maint.DefaultConfig()), Deps{}); err == nil {
		t.Fatal("ports")
	}
}

func TestASessionGoesOnAfterARestart(t *testing.T) {
	store := &memStore{docs: map[string][]byte{}}
	clock := &fakeClock{now: t0, timers: map[*fakeTimer]bool{}}
	r := start(t, store, clock)
	r.open("m1")
	r.waitFor("the tunnel", func() bool { return r.tun.count() == 1 })
	close(r.tun.last().up)
	r.steps("opening", "open")
	r.stop()
	if !r.tun.last().isStopped() {
		t.Fatal("the tunnel closes with the service")
	}
	r2 := start(t, store, clock)
	r2.waitFor("the tunnel again", func() bool { return r2.tun.count() == 1 })
	clock.Advance(time.Hour)
	r2.steps("closed/timeout")
	if s := r2.broker.msgs[0]; s.DurationS != 3600 || s.User != "kevin" {
		t.Fatalf("%+v", s)
	}
}
