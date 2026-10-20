package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/gsm"
	"safehome/internal/schematest"
)

const site = "tg-lom-0001"

var t0 = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

// --- fakes ---------------------------------------------------------------------------

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

type published struct {
	topic  string
	retain bool
	raw    []byte
	env    contract.Envelope
}

type fakeBroker struct {
	mu   sync.Mutex
	msgs []published
}

func (b *fakeBroker) Publish(_ context.Context, topic string, payload []byte, _ byte, retain bool) error {
	env, err := contract.ParseEnvelope(payload)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, published{topic: topic, retain: retain, raw: payload, env: env})
	return nil
}

func (b *fakeBroker) ofType(typ string) []published {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []published
	for _, m := range b.msgs {
		if m.env.Type == typ {
			out = append(out, m)
		}
	}
	return out
}

type sms struct{ number, text string }

type fakeModem struct {
	mu       sync.Mutex
	sent     []sms
	calls    []string
	checks   int
	restarts int
	sendErr  error
	checkErr error
	info     LineInfo
	ref      int
}

func (m *fakeModem) SendSMS(_ context.Context, number, text string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendErr != nil {
		return "", m.sendErr
	}
	m.sent = append(m.sent, sms{number, text})
	m.ref++
	return fmt.Sprint(m.ref), nil
}

func (m *fakeModem) Call(_ context.Context, number string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, number)
	return m.sendErr
}

func (m *fakeModem) Check(context.Context) (LineInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checks++
	return m.info, m.checkErr
}

func (m *fakeModem) Restart(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restarts++
	m.checkErr = nil // the restart cured it
	return nil
}

func (m *fakeModem) count() (sent, calls, checks, restarts int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent), len(m.calls), m.checks, m.restarts
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

// --- rig -----------------------------------------------------------------------------

type rig struct {
	t      *testing.T
	gw     *Gateway
	clock  *fakeClock
	store  *memStore
	broker *fakeBroker
	modem  *fakeModem
	logs   *syncBuffer
	ids    *contract.IDGenerator
	cancel context.CancelFunc
	done   chan struct{}
}

func newRig(t *testing.T, store *memStore, mutate ...func(*Config)) *rig {
	t.Helper()
	if store == nil {
		store = &memStore{docs: map[string][]byte{}}
	}
	r := &rig{
		t: t, clock: &fakeClock{now: t0, timers: map[*fakeTimer]bool{}}, store: store, broker: &fakeBroker{},
		modem: &fakeModem{info: LineInfo{Registered: true, Signal: 71, Operator: "TOGOCOM"}}, logs: &syncBuffer{},
		ids: contract.NewIDGenerator(),
	}
	cfg := DefaultConfig(site, gsm.DefaultConfig([]gsm.Line{{ID: "line1"}}, map[string]string{"u_owner": "90 12 34 56"}))
	cfg.RetryMin, cfg.RetryMax = time.Millisecond, 5*time.Millisecond
	for _, m := range mutate {
		m(&cfg)
	}
	gw, err := New(cfg, Deps{
		Clock: r.clock, Store: store, Publisher: r.broker, NewID: r.ids.New,
		Logger: slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Lines:  map[string]Modem{"line1": r.modem},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.gw = gw
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	go func() { gw.Run(ctx); close(r.done) }()
	t.Cleanup(r.stop)
	r.waitFor("the line to be usable", func() bool {
		st := r.status()
		return len(st.Lines) == 1 && st.Lines[0].Registered
	})
	return r
}

var stopOnce sync.Map

func (r *rig) stop() {
	if _, done := stopOnce.LoadOrStore(r, true); done {
		return
	}
	r.cancel()
	<-r.done
}

func (r *rig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s\nlogs:\n%s", what, r.logs.String())
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *rig) status() contract.GSMStatusData {
	all := r.broker.ofType(contract.TypeGSMStatus)
	if len(all) == 0 {
		return contract.GSMStatusData{}
	}
	d, _ := contract.DecodeData[contract.GSMStatusData](all[len(all)-1].env)
	return d
}

func (r *rig) request(channel, contact, kind string) string {
	r.t.Helper()
	id := r.ids.New()
	env, err := contract.NewEnvelope(site, "escalation", 1, id, r.clock.Now(), contract.TypeAlertRequest, contract.AlertRequestData{
		Incident: "inc1", Kind: kind, Urgency: contract.UrgencyCritical, Contact: contact, Channel: channel,
		Text: "ALARME Boutique: intrusion a 21:04. Repondre 1 si vous gerez.",
	})
	if err != nil {
		r.t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	r.gw.HandleMessage(Message{Topic: contract.AlertRequestTopic(site), Payload: b})
	return id
}

func (r *rig) results(ref string) []string {
	var out []string
	for _, m := range r.broker.ofType(contract.TypeAlertResult) {
		d, _ := contract.DecodeData[contract.AlertResultData](m.env)
		if d.Ref == ref {
			out = append(out, d.Status+"/"+d.Reason)
		}
	}
	return out
}

func (r *rig) validateAll() {
	r.t.Helper()
	files := map[string]string{
		contract.TypeAlertResult: "alert-result.json", contract.TypeAlertAck: "alert-ack.json", contract.TypeGSMStatus: "gsm-status.json",
	}
	r.broker.mu.Lock()
	msgs := append([]published(nil), r.broker.msgs...)
	r.broker.mu.Unlock()
	for _, m := range msgs {
		if err := schematest.Check(files[m.env.Type], m.env.Data); err != nil {
			r.t.Errorf("%s: %v", m.env.Type, err)
		}
		if err := schematest.Check("envelope.json", m.raw); err != nil {
			r.t.Errorf("envelope: %v", err)
		}
		if m.env.Src != "gsm" {
			r.t.Errorf("src %q", m.env.Src)
		}
	}
	// No text and no phone number in the logs.
	if l := r.logs.String(); strings.Contains(l, "ALARME") || strings.Contains(l, "90123456") {
		r.t.Errorf("the logs leak a text or a number:\n%s", l)
	}
}

// waitTimer waits until something waits on the clock for the given time.
func (r *rig) waitTimer(when time.Time) {
	r.t.Helper()
	r.waitFor("a timer at "+when.String(), func() bool {
		r.clock.mu.Lock()
		defer r.clock.mu.Unlock()
		for t := range r.clock.timers {
			if t.when.Equal(when) {
				return true
			}
		}
		return false
	})
}
