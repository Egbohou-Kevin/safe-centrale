package mqtt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"

	"safehome/internal/adapter/mqtt/mqtttest"
)

func TestOptionsValidation(t *testing.T) {
	good := Options{
		BrokerURL: "mqtt://127.0.0.1:1883", ClientID: "safehome-hub",
		Subscriptions: []Subscription{{Filter: "safehome/tg-lom-0001/#", QoS: 1}},
		OnMessage:     func(Message) {},
	}
	if _, err := good.validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Options){
		"empty URL":          func(o *Options) { o.BrokerURL = "" },
		"no host":            func(o *Options) { o.BrokerURL = "mqtt://" },
		"wrong scheme":       func(o *Options) { o.BrokerURL = "http://127.0.0.1:1883" },
		"no client id":       func(o *Options) { o.ClientID = "" },
		"no handler":         func(o *Options) { o.OnMessage = nil },
		"two handlers":       func(o *Options) { o.Take = func(Message) bool { return true } },
		"empty filter":       func(o *Options) { o.Subscriptions = []Subscription{{Filter: "", QoS: 1}} },
		"QoS above 2":        func(o *Options) { o.Subscriptions = []Subscription{{Filter: "a", QoS: 3}} },
		"unparsable URL":     func(o *Options) { o.BrokerURL = "mqtt://[::1" },
		"URL without scheme": func(o *Options) { o.BrokerURL = "127.0.0.1:1883" },
		"bad fallback":       func(o *Options) { o.FallbackURLs = []string{"https://cloud.example:443"} },
		"password in URL":    func(o *Options) { o.BrokerURL = "mqtts://site:secret@cloud.example:8883" },
		"missing CA file":    func(o *Options) { o.CAFile = "/nonexistent/ca.pem" },
		"empty CA file":      func(o *Options) { o.CAFile = emptyFile(t) },
	}
	for name, mutate := range bad {
		o := good
		mutate(&o)
		_, verr := o.validate()
		_, terr := o.tlsConfig()
		if verr == nil && terr == nil {
			t.Errorf("%s: want an error", name)
		}
		if _, err := Dial(context.Background(), o); err == nil {
			t.Errorf("%s: Dial must refuse", name)
		}
	}

	// Take instead of OnMessage.
	take := good
	take.OnMessage, take.Take = nil, func(Message) bool { return true }
	if _, err := take.validate(); err != nil {
		t.Fatal(err)
	}

	// A URL without a port gets its scheme's usual one: the dial needs one.
	for raw, host := range map[string]string{
		"mqtt://127.0.0.1": "127.0.0.1:1883", "mqtts://cloud.example": "cloud.example:8883",
		"tls://cloud.example": "cloud.example:8883", "wss://cloud.example/mqtt": "cloud.example:443",
		"ws://[::1]/mqtt": "[::1]:80", "mqtt://127.0.0.1:1885": "127.0.0.1:1885",
	} {
		if u, err := parseBrokerURL(raw); err != nil || u.Host != host {
			t.Errorf("%s: %v %v, want %s", raw, u, err, host)
		}
	}

	// A publish-only client needs no handler.
	pub := Options{BrokerURL: "mqtt://127.0.0.1:1883", ClientID: "x"}
	if _, err := pub.validate(); err != nil {
		t.Fatalf("a client that only publishes needs no handler: %v", err)
	}
}

func TestBackoffGrowsThenStops(t *testing.T) {
	// Attempt 0 is the first connection: no wait (it cost every start a second).
	want := []time.Duration{0, 1, 2, 4, 8, 16, 30, 30, 30}
	for n, w := range want {
		if got := backoff(n); got != w*time.Second {
			t.Errorf("backoff(%d) = %v, want %v", n, got, w*time.Second)
		}
	}
	if backoff(-3) != 0 {
		t.Error("a negative attempt count must not break the delay")
	}
	if backoff(1000) != 30*time.Second {
		t.Error("a large attempt count must not overflow")
	}
}

func TestSpreadDelaysStayUnderTheBackoffAndDiffer(t *testing.T) {
	if reconnectDelay(false)(3) != 4*time.Second {
		t.Fatal("without spread, the plain backoff")
	}
	spread := reconnectDelay(true)
	for _, n := range []int{0, 1, 3, 10} {
		limit := backoff(n)
		if n == 0 {
			limit = firstSpread
		}
		seen := map[time.Duration]bool{}
		for range 200 {
			d := spread(n)
			if d < 0 || d > limit {
				t.Fatalf("attempt %d: %v outside [0, %v]", n, d, limit)
			}
			seen[d] = true
		}
		if len(seen) < 150 {
			t.Errorf("attempt %d: only %d different delays out of 200", n, len(seen))
		}
	}
}

func TestPacketTranslation(t *testing.T) {
	m := Message{Topic: "safehome/s/alarm/state", Payload: []byte(`{}`), QoS: 1, Retain: true}
	p := publishPacket(m)
	if p.Topic != m.Topic || string(p.Payload) != "{}" || p.QoS != 1 || !p.Retain {
		t.Fatalf("%+v", p)
	}
	back := fromPaho(p)
	if back.Topic != m.Topic || string(back.Payload) != string(m.Payload) || back.QoS != 1 || !back.Retain {
		t.Fatalf("round trip: %+v", back)
	}

	s := subscribePacket([]Subscription{{Filter: "a/#", QoS: 1}, {Filter: "b/+/c", QoS: 0}})
	if len(s.Subscriptions) != 2 || s.Subscriptions[0].Topic != "a/#" || s.Subscriptions[0].QoS != 1 ||
		s.Subscriptions[1].Topic != "b/+/c" || s.Subscriptions[1].QoS != 0 {
		t.Fatalf("%+v", s.Subscriptions)
	}
}

func TestReasonCodes(t *testing.T) {
	for code, bad := range map[byte]bool{0x00: false, 0x01: false, 0x10: false /* no subscribers */, 0x80: true, 0x87: true, 0x97: true} {
		if failed(code) != bad {
			t.Errorf("failed(%#x) = %v", code, !bad)
		}
	}
	for code, final := range map[byte]bool{0x00: false, 0x10: false, 0x80: true, 0x87: true, 0x90: true, 0x91: false, 0x97: false, 0x99: true} {
		if refused(code) != final {
			t.Errorf("refused(%#x) = %v", code, !final)
		}
	}
}

func TestPublishBeforeAnyBrokerFails(t *testing.T) {
	// Nothing listens on this port: the client keeps retrying in the background and
	// Publish says so at once, instead of blocking an alarm event.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := Dial(ctx, Options{BrokerURL: "mqtt://127.0.0.1:1", ClientID: "safehome-test"})
	if err != nil {
		t.Fatal(err)
	}
	pubCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	err = c.Publish(pubCtx, Message{Topic: "t", QoS: 1})
	if !errors.Is(err, ErrDown) {
		t.Fatalf("got %v, want ErrDown", err)
	}

	awaitCtx, stopAwait := context.WithTimeout(ctx, 300*time.Millisecond)
	defer stopAwait()
	if err := c.Await(awaitCtx); err == nil {
		t.Fatal("no broker: Await must not succeed")
	}
}

// --- against a real broker (a private Mosquitto, skipped if not installed) ----------

var clientSeq atomic.Int64

func uniqueID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), clientSeq.Add(1))
}

// collector gathers the messages a client receives.
type collector struct {
	mu   sync.Mutex
	msgs []Message
	got  chan struct{}
}

func newCollector() *collector { return &collector{got: make(chan struct{}, 256)} }

func (c *collector) handle(m Message) {
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
	c.got <- struct{}{}
}

func (c *collector) wait(t *testing.T, n int) []Message {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		c.mu.Lock()
		have := len(c.msgs)
		c.mu.Unlock()
		if have >= n {
			c.mu.Lock()
			defer c.mu.Unlock()
			return append([]Message(nil), c.msgs...)
		}
		select {
		case <-c.got:
		case <-deadline:
			t.Fatalf("timed out: %d message(s) received, wanted %d", have, n)
		}
	}
}

func dial(t *testing.T, ctx context.Context, url, id string, subs []Subscription, h func(Message)) *Client {
	t.Helper()
	// A connected client may not be subscribed yet: a message published before the
	// broker's answer would be lost, so a client with subscriptions waits for it.
	subscribed := make(chan struct{}, 1)
	o := Options{BrokerURL: url, ClientID: id, Subscriptions: subs, OnMessage: h}
	if len(subs) > 0 {
		o.OnSubscribed = func() {
			select {
			case subscribed <- struct{}{}:
			default:
			}
		}
	}
	c, err := Dial(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	awaitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.Await(awaitCtx); err != nil {
		t.Fatalf("connecting to the broker: %v", err)
	}
	if len(subs) > 0 {
		select {
		case <-subscribed:
		case <-awaitCtx.Done():
			t.Fatalf("subscribing at the broker: %v", awaitCtx.Err())
		}
	}
	return c
}

func TestPublishAndReceiveThroughARealBroker(t *testing.T) {
	broker := mqtttest.StartOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got := newCollector()
	sub := dial(t, ctx, broker.URL(), uniqueID("sub"), []Subscription{{Filter: "safehome/s1/device/+/event", QoS: 1}}, got.handle)
	pub := dial(t, ctx, broker.URL(), uniqueID("pub"), nil, nil)

	if err := pub.Publish(ctx, Message{Topic: "safehome/s1/device/dev_a/event", Payload: []byte(`{"n":1}`), QoS: 1}); err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(ctx, Message{Topic: "safehome/s1/device/dev_b/event", Payload: []byte(`{"n":2}`), QoS: 1}); err != nil {
		t.Fatal(err)
	}
	// This one matches no subscription and must not arrive.
	if err := pub.Publish(ctx, Message{Topic: "safehome/s1/alarm/event", Payload: []byte(`{"n":3}`), QoS: 1}); err != nil {
		t.Fatal(err)
	}

	msgs := got.wait(t, 2)
	time.Sleep(100 * time.Millisecond)
	msgs = got.wait(t, 2)
	if len(msgs) != 2 || msgs[0].Topic != "safehome/s1/device/dev_a/event" || string(msgs[0].Payload) != `{"n":1}` ||
		msgs[1].Topic != "safehome/s1/device/dev_b/event" {
		t.Fatalf("%+v", msgs)
	}
	sub.Close(ctx)
	pub.Close(ctx)
}

func TestRetainedMessagesReachALateSubscriber(t *testing.T) {
	broker := mqtttest.StartOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pub := dial(t, ctx, broker.URL(), uniqueID("pub"), nil, nil)
	topic := "safehome/s1/alarm/state"
	if err := pub.Publish(ctx, Message{Topic: topic, Payload: []byte(`{"state":"armed"}`), QoS: 1, Retain: true}); err != nil {
		t.Fatal(err)
	}

	late := newCollector()
	dial(t, ctx, broker.URL(), uniqueID("late"), []Subscription{{Filter: topic, QoS: 1}}, late.handle)
	msgs := late.wait(t, 1)
	if string(msgs[0].Payload) != `{"state":"armed"}` || !msgs[0].Retain {
		t.Fatalf("%+v", msgs[0])
	}
}

func TestTheClientReconnectsAndSubscribesAgain(t *testing.T) {
	broker := mqtttest.StartOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	got := newCollector()
	dial(t, ctx, broker.URL(), uniqueID("sub"), []Subscription{{Filter: "safehome/s1/#", QoS: 1}}, got.handle)

	// The broker dies and comes back with no memory of the client's subscription.
	broker.Stop()
	broker.Restart()

	pub := dial(t, ctx, broker.URL(), uniqueID("pub"), nil, nil)
	// The subscriber reconnects on its own after its backoff (1 s, then 2 s...):
	// keep publishing until a message gets through.
	deadline := time.Now().Add(30 * time.Second)
	for n := 0; ; n++ {
		if err := pub.Publish(ctx, Message{Topic: "safehome/s1/alarm/event", Payload: []byte(fmt.Sprintf(`{"n":%d}`, n)), QoS: 1}); err != nil {
			t.Fatal(err)
		}
		got.mu.Lock()
		received := len(got.msgs)
		got.mu.Unlock()
		if received > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the subscriber never came back after the broker restarted")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func TestPublishFailsFastWhileTheBrokerIsDownThenWorksAgain(t *testing.T) {
	broker := mqtttest.StartOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c := dial(t, ctx, broker.URL(), uniqueID("pub"), nil, nil)
	broker.Stop()

	// The client notices the loss within a keepalive period or on its next write.
	deadline := time.Now().Add(20 * time.Second)
	for {
		pubCtx, stop := context.WithTimeout(ctx, 3*time.Second)
		err := c.Publish(pubCtx, Message{Topic: "safehome/s1/alarm/event", Payload: []byte(`{}`), QoS: 1})
		stop()
		if errors.Is(err, ErrDown) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Publish never reported the broker as down (last: %v)", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	broker.Restart()
	awaitCtx, stopAwait := context.WithTimeout(ctx, 20*time.Second)
	defer stopAwait()
	if err := c.Await(awaitCtx); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(ctx, Message{Topic: "safehome/s1/alarm/event", Payload: []byte(`{}`), QoS: 1}); err != nil {
		t.Fatalf("publishing after the broker came back: %v", err)
	}
}

func TestCloseDisconnectsCleanly(t *testing.T) {
	broker := mqtttest.StartOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := dial(t, ctx, broker.URL(), uniqueID("c"), nil, nil)
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestOnSubscribedFiresOnceTheSubscriptionsAreActive(t *testing.T) {
	broker := mqtttest.StartOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for name, subs := range map[string][]Subscription{
		"with subscriptions":    {{Filter: "safehome/s1/#", QoS: 1}},
		"without subscriptions": nil,
	} {
		up := make(chan struct{}, 4)
		c, err := Dial(ctx, Options{
			BrokerURL: broker.URL(), ClientID: uniqueID("up"), Subscriptions: subs,
			OnMessage: func(Message) {}, OnSubscribed: func() { up <- struct{}{} },
		})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-up:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: OnSubscribed was never called", name)
		}
		c.Close(ctx)
	}
}

func TestAnEphemeralClientFindsNothingQueuedFromItsLastRun(t *testing.T) {
	broker := mqtttest.StartOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pub := dial(t, ctx, broker.URL(), uniqueID("pub"), nil, nil)
	subs := []Subscription{{Filter: "safehome/s1/registry/event", QoS: 1}}

	for _, ephemeral := range []bool{false, true} {
		id := uniqueID("tool")
		connect := func(h func(Message)) *Client {
			c, err := Dial(ctx, Options{BrokerURL: broker.URL(), ClientID: id, Subscriptions: subs, OnMessage: h, Ephemeral: ephemeral})
			if err != nil {
				t.Fatal(err)
			}
			awaitCtx, stop := context.WithTimeout(ctx, 10*time.Second)
			defer stop()
			if err := c.Await(awaitCtx); err != nil {
				t.Fatal(err)
			}
			return c
		}
		first := newCollector()
		c := connect(first.handle)
		time.Sleep(200 * time.Millisecond) // let the subscription settle
		c.Close(ctx)

		// Published while the client is away.
		if err := pub.Publish(ctx, Message{Topic: "safehome/s1/registry/event", Payload: []byte(`{"n":1}`), QoS: 1}); err != nil {
			t.Fatal(err)
		}

		again := newCollector()
		c = connect(again.handle)
		time.Sleep(500 * time.Millisecond)
		again.mu.Lock()
		n := len(again.msgs)
		again.mu.Unlock()
		if want := map[bool]int{false: 1, true: 0}[ephemeral]; n != want {
			t.Errorf("ephemeral=%v: %d message(s) waiting at reconnection, want %d", ephemeral, n, want)
		}
		c.Close(ctx)
	}
}

func emptyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// links records what OnLinkChange says.
type links struct {
	mu  sync.Mutex
	log []string
	ch  chan string
}

func newLinks() *links { return &links{ch: make(chan string, 16)} }

func (l *links) change(up bool, scheme string) {
	s := "down"
	if up {
		s = "up " + scheme
	}
	l.mu.Lock()
	l.log = append(l.log, s)
	l.mu.Unlock()
	l.ch <- s
}

func (l *links) next(t *testing.T) string {
	t.Helper()
	select {
	case s := <-l.ch:
		return s
	case <-time.After(15 * time.Second):
		t.Fatal("no link change")
		return ""
	}
}

func TestNoLocalKeepsAClientDeafToItself(t *testing.T) {
	broker := mqtttest.StartOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, noLocal := range []bool{false, true} {
		got := newCollector()
		c := dial(t, ctx, broker.URL(), uniqueID("self"), []Subscription{{Filter: "safehome/s1/alert/result", QoS: 1, NoLocal: noLocal}}, got.handle)
		time.Sleep(100 * time.Millisecond)
		if err := c.Publish(ctx, Message{Topic: "safehome/s1/alert/result", Payload: []byte(`{}`), QoS: 1}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		got.mu.Lock()
		n := len(got.msgs)
		got.mu.Unlock()
		if want := map[bool]int{false: 1, true: 0}[noLocal]; n != want {
			t.Errorf("NoLocal=%v: heard itself %d time(s), want %d", noLocal, n, want)
		}
		c.Close(ctx)
	}
}

func TestTheFallbackURLIsUsedWhenTheFirstIsBlocked(t *testing.T) {
	broker := mqtttest.StartOpenWith(t, mqtttest.Options{WebSockets: true})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	l := newLinks()
	got := newCollector()
	c, err := Dial(ctx, Options{
		BrokerURL: "mqtt://127.0.0.1:1", FallbackURLs: []string{broker.WSURL()}, // port 1: "blocked"
		ClientID: uniqueID("ws"), OnLinkChange: l.change,
		Subscriptions: []Subscription{{Filter: "safehome/s1/alarm/cmd", QoS: 1}}, OnMessage: got.handle,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := l.next(t); s != "up ws" {
		t.Fatalf("link %q, want up over ws", s)
	}
	pub := dial(t, ctx, broker.URL(), uniqueID("pub"), nil, nil)
	if err := pub.Publish(ctx, Message{Topic: "safehome/s1/alarm/cmd", Payload: []byte(`{"n":1}`), QoS: 1}); err != nil {
		t.Fatal(err)
	}
	if msgs := got.wait(t, 1); string(msgs[0].Payload) != `{"n":1}` {
		t.Fatalf("%+v", msgs)
	}
	c.Close(ctx)
}

func TestSecureWebSocketsAsTheCloudFallback(t *testing.T) {
	files := mqtttest.NewTLSFiles(t)
	broker := mqtttest.StartOpenWith(t, mqtttest.Options{WebSockets: true, TLS: &files})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	l := newLinks()
	got := newCollector()
	c, err := Dial(ctx, Options{
		BrokerURL: "mqtts://localhost:1", FallbackURLs: []string{broker.WSSURL()}, CAFile: files.CA,
		ClientID: uniqueID("wss"), OnLinkChange: l.change,
		Subscriptions: []Subscription{{Filter: "safehome/s1/alarm/cmd", QoS: 1, NoLocal: true}}, OnMessage: got.handle,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := l.next(t); s != "up wss" {
		t.Fatalf("link %q, want up over wss", s)
	}
	// A large message, written in several pieces by the library, still goes as one.
	big := []byte(`{"text":"` + strings.Repeat("x", 70000) + `"}`)
	if err := c.Publish(ctx, Message{Topic: "safehome/s1/system/health", Payload: big, QoS: 1, Retain: true}); err != nil {
		t.Fatal(err)
	}
	late := newCollector()
	dial(t, ctx, broker.URL(), uniqueID("late"), []Subscription{{Filter: "safehome/s1/system/health", QoS: 1}}, late.handle)
	if msgs := late.wait(t, 1); len(msgs[0].Payload) != len(big) {
		t.Fatalf("%d bytes arrived, %d sent", len(msgs[0].Payload), len(big))
	}
	c.Close(ctx)
}

func TestTLSTrustsOnlyTheGivenAuthority(t *testing.T) {
	files := mqtttest.NewTLSFiles(t)
	broker := mqtttest.StartOpenWith(t, mqtttest.Options{TLS: &files})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	l := newLinks()
	c, err := Dial(ctx, Options{BrokerURL: broker.TLSURL(), CAFile: files.CA, ClientID: uniqueID("tls"), OnLinkChange: l.change})
	if err != nil {
		t.Fatal(err)
	}
	if s := l.next(t); s != "up mqtts" {
		t.Fatalf("link %q", s)
	}
	if err := c.Publish(ctx, Message{Topic: "safehome/s1/system/health", Payload: []byte(`{}`), QoS: 1}); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)

	// Without the authority, the test certificate is refused: no connection.
	untrusting, err := Dial(ctx, Options{BrokerURL: broker.TLSURL(), ClientID: uniqueID("tls")})
	if err != nil {
		t.Fatal(err)
	}
	awaitCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	if err := untrusting.Await(awaitCtx); err == nil {
		t.Fatal("connected to a broker whose certificate nobody trusts")
	}
}

// proxy sits between a client and the broker; drop cuts every connection without a
// word, as a dead 4G link would.
type proxy struct {
	l     net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func startProxy(t *testing.T, target string) *proxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{l: l}
	t.Cleanup(func() { l.Close(); p.drop() })
	go func() {
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				in.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, in, out)
			p.mu.Unlock()
			go func() { io.Copy(out, in); out.Close() }()
			go func() { io.Copy(in, out); in.Close() }()
		}
	}()
	return p
}

func (p *proxy) url() string { return "mqtt://" + p.l.Addr().String() }

func (p *proxy) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetLinger(0) // a reset, not a polite close
		}
		c.Close()
	}
	p.conns = nil
}

func TestTheWillIsPublishedWhenTheLinkDiesAndTheLinkChangeIsTold(t *testing.T) {
	broker := mqtttest.StartOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p := startProxy(t, strings.TrimPrefix(broker.URL(), "mqtt://"))

	watcher := newCollector()
	dial(t, ctx, broker.URL(), uniqueID("cloud"), []Subscription{{Filter: "safehome/s1/agent/status", QoS: 1}}, watcher.handle)

	var wills atomic.Int32
	l := newLinks()
	c, err := Dial(ctx, Options{
		BrokerURL: p.url(), ClientID: uniqueID("agent"), OnLinkChange: l.change,
		Will: func() Message {
			n := wills.Add(1)
			return Message{Topic: "safehome/s1/agent/status", Payload: []byte(fmt.Sprintf(`{"uplink":"down","n":%d}`, n)), QoS: 1, Retain: true}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := l.next(t); s != "up mqtt" {
		t.Fatalf("link %q", s)
	}
	p.drop()
	if s := l.next(t); s != "down" {
		t.Fatalf("link %q, want down", s)
	}
	msgs := watcher.wait(t, 1)
	if !strings.Contains(string(msgs[0].Payload), `"uplink":"down"`) {
		t.Fatalf("will %s", msgs[0].Payload)
	}
	// It comes back by itself, with a fresh will.
	if s := l.next(t); s != "up mqtt" {
		t.Fatalf("link %q, want up again", s)
	}
	if wills.Load() < 2 {
		t.Fatalf("the will was built %d time(s), want once per connection", wills.Load())
	}
	c.Close(ctx)
}

// A broker that refuses the client is said apart, with its reason: deploy/update-hub.sh
// looks for these words. Any other failure stays a warning.
func TestARefusalOfTheBrokerIsSaidApart(t *testing.T) {
	var out strings.Builder
	s := &session{log: slog.New(slog.NewTextHandler(&out, nil))}
	s.connectError(autopaho.NewConnackError(errors.New("failed to connect to server: "), &paho.Connack{ReasonCode: 135}))
	s.connectError(errors.New("dial tcp 127.0.0.1:1883: connect: connection refused"))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 ||
		!strings.Contains(lines[0], `level=ERROR msg="mqtt broker refused the connection" reason=135`) ||
		!strings.Contains(lines[1], `level=WARN msg="mqtt connection attempt failed"`) {
		t.Fatalf("logged:\n%s", out.String())
	}
}
