package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/gsm"
	"safehome/internal/inbox"
	"safehome/internal/outbox/outboxtest"
	"safehome/internal/schematest"
)

const site = "tg-lom-0001"

var t0 = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

// --- fakes ---------------------------------------------------------------------------

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

// --- rig -----------------------------------------------------------------------------

type rig struct {
	t      *testing.T
	gw     *Gateway
	clock  *drivertest.FakeClock
	store  *drivertest.MemStore
	broker *outboxtest.Broker
	modem  *fakeModem
	logs   *drivertest.SyncBuffer
	ids    *contract.IDGenerator
	cancel context.CancelFunc
	done   chan struct{}
}

func newRig(t *testing.T, store *drivertest.MemStore, mutate ...func(*Config)) *rig {
	t.Helper()
	if store == nil {
		store = drivertest.NewMemStore()
	}
	r := &rig{
		t: t, clock: drivertest.NewFakeClock(t0), store: store, broker: &outboxtest.Broker{},
		modem: &fakeModem{info: LineInfo{Registered: true, Signal: 71, Operator: "TOGOCOM"}}, logs: drivertest.Logs(t),
		ids: contract.NewIDGenerator(),
	}
	cfg := DefaultConfig(site, gsm.DefaultConfig([]gsm.Line{{ID: "line1"}}, map[string]string{"u_owner": "90 12 34 56"}))
	cfg.Outbox.RetryMin, cfg.Outbox.RetryMax = time.Millisecond, 5*time.Millisecond
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
	drivertest.WaitFor(r.t, "the line to be usable", func() bool {
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

// settle waits until the loop has handled what was given to it, and the line has
// done what the loop handed it: the line's worker takes its tasks in order.
func (r *rig) settle() {
	r.t.Helper()
	r.gw.loop.Do(context.Background(), func(time.Time) []gsm.Action { return nil })
	done := make(chan struct{})
	r.gw.toLine("line1", func(context.Context) { close(done) })
	drivertest.WaitFor(r.t, "the line's work", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
}

func (r *rig) status() contract.GSMStatusData {
	all := r.broker.OfType(contract.TypeGSMStatus)
	if len(all) == 0 {
		return contract.GSMStatusData{}
	}
	d, _ := contract.DecodeData[contract.GSMStatusData](all[len(all)-1].Env)
	return d
}

func (r *rig) request(channel, contact, kind string) string {
	r.t.Helper()
	return r.requestAt(r.clock.Now(), channel, contact, kind)
}

// requestAt sends an alert request of incident inc1 written at ts, and returns its id.
func (r *rig) requestAt(ts time.Time, channel, contact, kind string) string {
	r.t.Helper()
	id := r.ids.New()
	r.publish(contract.AlertRequestTopic(site), id, ts, contract.TypeAlertRequest, contract.AlertRequestData{
		Incident: "inc1", Kind: kind, Urgency: contract.UrgencyCritical, Contact: contact, Channel: channel,
		Text: "ALARME Boutique: intrusion a 21:04. Repondre 1 si vous gerez.",
	})
	return id
}

// publish hands the gateway a message of the site as the broker would.
func (r *rig) publish(topic, id string, ts time.Time, typ string, data any) {
	r.t.Helper()
	env, err := contract.NewEnvelope(site, "hub", 1, id, ts, typ, data)
	if err != nil {
		r.t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	r.gw.HandleMessage(inbox.Message{Topic: topic, Payload: b})
}

func (r *rig) results(ref string) []string {
	var out []string
	for _, m := range r.broker.OfType(contract.TypeAlertResult) {
		d, _ := contract.DecodeData[contract.AlertResultData](m.Env)
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
	for _, m := range r.broker.All() {
		if err := schematest.Check(files[m.Env.Type], m.Env.Data); err != nil {
			r.t.Errorf("%s: %v", m.Env.Type, err)
		}
		if err := schematest.Check("envelope.json", m.Payload); err != nil {
			r.t.Errorf("envelope: %v", err)
		}
		if m.Env.Src != "gsm" {
			r.t.Errorf("src %q", m.Env.Src)
		}
	}
	// No text and no phone number in the logs.
	if l := r.logs.String(); strings.Contains(l, "ALARME") || strings.Contains(l, "90123456") {
		r.t.Errorf("the logs leak a text or a number:\n%s", l)
	}
}
