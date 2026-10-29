package maintapp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/inbox"
	"safehome/internal/maint"
	"safehome/internal/outbox/outboxtest"
	"safehome/internal/schematest"
)

const site = "tg-lom-0001"

var t0 = time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)

// --- fakes ---------------------------------------------------------------------

// broker is the shared fake broker that refuses anything but a valid maint.session,
// not retained.
type broker struct{ outboxtest.Broker }

func (b *broker) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	env, err := contract.ParseEnvelope(payload)
	if err != nil || topic != contract.MaintSessionTopic(site) || env.Type != contract.TypeMaintSession || retain {
		return errors.New("unexpected " + topic)
	}
	if err := schematest.Check("maint-session.json", env.Data); err != nil {
		return err
	}
	return b.Broker.Publish(ctx, topic, payload, qos, retain)
}

func (b *broker) sessions() []contract.MaintSessionData {
	var out []contract.MaintSessionData
	for _, m := range b.All() {
		d, _ := contract.DecodeData[contract.MaintSessionData](m.Env)
		out = append(out, d)
	}
	return out
}

func (b *broker) steps() []string {
	var out []string
	for _, d := range b.sessions() {
		out = append(out, strings.TrimSuffix(d.Status+"/"+d.Reason, "/"))
	}
	return out
}

type fakeTunnel struct {
	up, done chan struct{}
	hold     chan struct{} // Stop waits for it, as ssh takes a moment to exit
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
	if t.hold != nil {
		<-t.hold
	}
	t.once.Do(func() { close(t.done) })
}
func (t *fakeTunnel) isStopped() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.stopped }

// breaks says the tunnel stopped by itself.
func (t *fakeTunnel) breaks() { t.once.Do(func() { close(t.done) }) }

type fakeTunneler struct {
	mu      sync.Mutex
	err     error
	flash   bool          // the tunnels it opens are up and already down
	hold    chan struct{} // given to the tunnels it opens (see fakeTunnel)
	overlap bool          // a tunnel was opened while another had not stopped
	tunnels []*fakeTunnel
}

func (f *fakeTunneler) Open(context.Context) (Tunnel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	for _, old := range f.tunnels {
		select {
		case <-old.done:
		default:
			f.overlap = true
		}
	}
	t := &fakeTunnel{up: make(chan struct{}), done: make(chan struct{}), hold: f.hold}
	if f.flash {
		close(t.up)
		t.breaks()
	}
	f.tunnels = append(f.tunnels, t)
	return t, nil
}

func (f *fakeTunneler) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.tunnels) }

func (f *fakeTunneler) last() *fakeTunnel {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tunnels[len(f.tunnels)-1]
}

// --- harness -------------------------------------------------------------------

type rig struct {
	t      *testing.T
	app    *App
	clock  *drivertest.FakeClock
	store  *drivertest.MemStore
	broker *broker
	tun    *fakeTunneler
	logs   *drivertest.SyncBuffer
	ids    *contract.IDGenerator
	cancel context.CancelFunc // stops the service; done is closed once it stopped
	done   chan struct{}
	stop   func() // stops the service and waits for its end
}

func start(t *testing.T, store *drivertest.MemStore, clock *drivertest.FakeClock, edit ...func(*maint.Config)) *rig {
	t.Helper()
	if store == nil {
		store = drivertest.NewMemStore()
	}
	if clock == nil {
		clock = drivertest.NewFakeClock(t0)
	}
	m := maint.DefaultConfig()
	for _, e := range edit {
		e(&m)
	}
	cfg := DefaultConfig(site, m)
	cfg.Outbox.RetryMin, cfg.Outbox.RetryMax = 5*time.Millisecond, 20*time.Millisecond
	r := &rig{t: t, clock: clock, store: store, broker: &broker{}, tun: &fakeTunneler{}, logs: drivertest.Logs(t), ids: contract.NewIDGenerator()}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("steps published: %v", r.broker.steps())
		}
	})
	app, err := New(cfg, Deps{Clock: clock, Store: store, Publisher: r.broker, Tunneler: r.tun, NewID: r.ids.New,
		Logger: slog.New(slog.NewTextHandler(r.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	r.app = app
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	go func() { app.Run(ctx); close(r.done) }()
	r.stop = func() { cancel(); <-r.done }
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
	r.app.HandleMessage(inbox.Message{Topic: contract.MaintCmdTopic(site), Payload: b})
	r.app.HandleMessage(inbox.Message{Topic: contract.MaintCmdTopic(site), Payload: b}) // QoS 1: again
}

func (r *rig) open(session string) {
	r.send(contract.TypeMaintOpen, contract.MaintOpenData{Session: session, User: "kevin", Reason: "modem", DurationS: 3600,
		Exp: contract.TS(r.clock.Now().Add(time.Minute))})
}

func (r *rig) steps(want ...string) {
	r.t.Helper()
	drivertest.WaitFor(r.t, strings.Join(want, " "), func() bool { return strings.Join(r.broker.steps(), " ") == strings.Join(want, " ") })
}

// settle lets the loop take what was given to it, and the outbox send what it
// published.
func (r *rig) settle() {
	r.t.Helper()
	r.app.Alive(context.Background())
	drivertest.WaitFor(r.t, "the outbox to empty", func() bool { return r.app.out.Depth() == 0 })
}

// --- tests ---------------------------------------------------------------------

func TestASessionFromOpenToClose(t *testing.T) {
	r := start(t, nil, nil)
	r.open("m1")
	drivertest.WaitFor(t, "the tunnel", func() bool { return r.tun.count() == 1 })
	r.steps("opening")
	close(r.tun.last().up)
	r.steps("opening", "open")
	r.send(contract.TypeMaintClose, contract.MaintCloseData{Session: "m1"})
	r.steps("opening", "open", "closed/stopped")
	drivertest.WaitFor(t, "the tunnel closed", r.tun.last().isStopped)
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
	drivertest.WaitFor(t, "the tunnel", func() bool { return r.tun.count() == 1 })
	close(r.tun.last().up)
	r.steps("opening", "open")
	r.clock.Advance(time.Hour)
	r.steps("opening", "open", "closed/timeout")
	drivertest.WaitFor(t, "the tunnel closed", r.tun.last().isStopped)
}

func TestATunnelThatBreaksIsOpenedAgain(t *testing.T) {
	r := start(t, nil, nil)
	r.open("m1")
	drivertest.WaitFor(t, "the tunnel", func() bool { return r.tun.count() == 1 })
	close(r.tun.last().up)
	r.steps("opening", "open")
	r.tun.last().breaks()
	drivertest.WaitFor(t, "the break said", func() bool { return strings.Contains(r.logs.String(), "tunnel stopped") })
	r.clock.PassTimer(t, 10*time.Second) // the log comes before the break reaches the core
	drivertest.WaitFor(t, "the second tunnel", func() bool { return r.tun.count() == 2 })
	close(r.tun.last().up)
	r.settle()
	r.steps("opening", "open")
}

// TestATunnelUpThenDownAtOnceIsOpenedAgain: when the tunnel is seen up and down at
// the same time, it was up: the session is open and the tunnel is tried again,
// it does not fail.
func TestATunnelUpThenDownAtOnceIsOpenedAgain(t *testing.T) {
	for range 20 {
		r := start(t, nil, nil)
		r.tun.flash = true
		r.open("m1")
		drivertest.WaitFor(r.t, "the break said", func() bool { return strings.Contains(r.logs.String(), "tunnel stopped") })
		r.settle()
		r.steps("opening", "open")
		r.stop()
	}
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
	drivertest.WaitFor(t, "the tunnel", func() bool { return r.tun.count() == 1 })
	r.tun.last().breaks() // never up
	r.steps("opening", "failed/unreachable", "opening", "failed/unreachable")
}

func TestRefusedOrders(t *testing.T) {
	r := start(t, nil, nil, func(c *maint.Config) { c.Enabled = false })
	r.open("m1")
	r.steps("failed/disabled")
	r.send(contract.TypeMaintOpen, contract.MaintOpenData{Session: "m2", User: "kevin", DurationS: 600}) // invalid: ignored
	r.send(contract.TypeMaintClose, contract.MaintCloseData{})
	r.app.HandleMessage(inbox.Message{Topic: contract.MaintCmdTopic(site), Payload: []byte("{")})
	r.app.HandleMessage(inbox.Message{Topic: "elsewhere", Payload: []byte("{}")})
	r.settle()
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
	store := drivertest.NewMemStore()
	clock := drivertest.NewFakeClock(t0)
	r := start(t, store, clock)
	r.open("m1")
	drivertest.WaitFor(t, "the tunnel", func() bool { return r.tun.count() == 1 })
	close(r.tun.last().up)
	r.steps("opening", "open")
	r.stop()
	if !r.tun.last().isStopped() {
		t.Fatal("the tunnel closes with the service")
	}
	r2 := start(t, store, clock)
	drivertest.WaitFor(t, "the tunnel again", func() bool { return r2.tun.count() == 1 })
	clock.Advance(time.Hour)
	r2.steps("closed/timeout")
	if s := r2.broker.sessions()[0]; s.DurationS != 3600 || s.User != "kevin" {
		t.Fatalf("%+v", s)
	}
}

// A message that arrives while the service stops (here during its last flush, which
// the broker holds) is given up without an error.
func TestMessagesDuringShutdownAreGivenUpQuietly(t *testing.T) {
	r := start(t, nil, nil)
	release := r.broker.Hold()
	// Something left for the last flush.
	r.app.out.Send(src, contract.MaintSessionTopic(site), contract.TypeMaintSession,
		contract.MaintSessionData{Session: "s0", Status: contract.MaintClosed, Reason: contract.MaintTimeout}, false, t0)
	r.cancel()
	drivertest.WaitFor(t, "the loop to stop", func() bool { return r.app.Alive(context.Background()) != nil })
	r.open("s2")
	release()
	<-r.done
	if strings.Contains(r.logs.String(), "level=ERROR") {
		t.Fatal("an error was logged for a message given up at shutdown")
	}
}

// TestANewTunnelWaitsForThePreviousToStop: a session closed then another opened at
// once must not start a second ssh while the first still holds the bastion's port:
// with ExitOnForwardFailure the new one would fail, and the session with it.
func TestANewTunnelWaitsForThePreviousToStop(t *testing.T) {
	r := start(t, nil, nil)
	release := make(chan struct{})
	r.tun.mu.Lock()
	r.tun.hold = release
	r.tun.mu.Unlock()
	r.open("m1")
	drivertest.WaitFor(t, "the tunnel", func() bool { return r.tun.count() == 1 })
	close(r.tun.last().up)
	r.steps("opening", "open")
	r.send(contract.TypeMaintClose, contract.MaintCloseData{Session: "m1"})
	drivertest.WaitFor(t, "the first tunnel stopping", r.tun.last().isStopped)
	r.open("m2")
	drivertest.WaitFor(t, "the second tunnel, or its wait", func() bool {
		return r.tun.count() == 2 || strings.Contains(r.logs.String(), "waiting for the previous tunnel")
	})
	close(release)
	drivertest.WaitFor(t, "the second tunnel", func() bool { return r.tun.count() == 2 })
	r.tun.mu.Lock()
	overlap := r.tun.overlap
	r.tun.mu.Unlock()
	if overlap {
		t.Fatal("the second ssh started before the first stopped")
	}
	close(r.tun.last().up)
	r.steps("opening", "open", "closed/stopped", "opening", "open")
}

// TestATunnelClosedWhileWaitingStillHoldsTheNext: a session closed while its tunnel
// waits for the previous one must not let a third tunnel start before that one stopped.
func TestATunnelClosedWhileWaitingStillHoldsTheNext(t *testing.T) {
	r := start(t, nil, nil)
	release := make(chan struct{})
	r.tun.mu.Lock()
	r.tun.hold = release
	r.tun.mu.Unlock()
	r.open("m1")
	drivertest.WaitFor(t, "the tunnel", func() bool { return r.tun.count() == 1 })
	close(r.tun.last().up)
	r.steps("opening", "open")
	r.send(contract.TypeMaintClose, contract.MaintCloseData{Session: "m1"})
	drivertest.WaitFor(t, "the first tunnel stopping", r.tun.last().isStopped)
	waits := func() int { return strings.Count(r.logs.String(), "waiting for the previous tunnel") }
	r.open("m2")
	drivertest.WaitFor(t, "the second tunnel waiting", func() bool { return waits() == 1 })
	r.send(contract.TypeMaintClose, contract.MaintCloseData{Session: "m2"})
	r.steps("opening", "open", "closed/stopped", "opening", "closed/stopped")
	r.open("m3")
	drivertest.WaitFor(t, "the third tunnel, or its wait", func() bool { return r.tun.count() == 2 || waits() == 2 })
	close(release)
	drivertest.WaitFor(t, "the third tunnel", func() bool { return r.tun.count() == 2 })
	r.tun.mu.Lock()
	overlap := r.tun.overlap
	r.tun.mu.Unlock()
	if overlap {
		t.Fatal("the third ssh started before the first stopped")
	}
	close(r.tun.last().up)
	r.steps("opening", "open", "closed/stopped", "opening", "closed/stopped", "opening", "open")
}
