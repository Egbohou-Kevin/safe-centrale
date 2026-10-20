// Package gateway is the application layer of the GSM gateway: it feeds the gsm
// domain with the alert requests that arrive from the broker and with what the
// modems say, makes the modems carry out what the domain decides, and publishes the
// results.
//
// Every modem is behind the Modem port, declared here and wired in the composition
// root (cmd/safehome-gsm), so the gateway is tested with fake modems. Each line has
// its own goroutine: one command at a time per modem, as AT modems require.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/gsm"
	gsmwire "safehome/internal/gsm/wire"
	"safehome/internal/outbox"
)

// Config holds what the gateway needs to know.
type Config struct {
	Site       string
	GSM        gsm.Config
	DedupeSize int
	OutboxSize int
	RetryMin   time.Duration
	RetryMax   time.Duration
	// StatusEvery is how often the status goes out even when nothing changed: the
	// hub takes a gateway that stopped repeating it for dead (see health.GSM).
	StatusEvery time.Duration
}

// DefaultConfig returns the defaults.
func DefaultConfig(site string, g gsm.Config) Config {
	return Config{
		Site: site, GSM: g, DedupeSize: 1024, OutboxSize: 500,
		RetryMin: 200 * time.Millisecond, RetryMax: 5 * time.Second,
		StatusEvery: time.Minute,
	}
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if !contract.ValidSite(c.Site) {
		return fmt.Errorf("gateway: invalid site %q", c.Site)
	}
	if c.DedupeSize < 1 || c.OutboxSize < 1 || c.RetryMin <= 0 || c.RetryMax < c.RetryMin || c.StatusEvery <= 0 {
		return errors.New("gateway: DedupeSize, OutboxSize, RetryMin, RetryMax and StatusEvery must be set")
	}
	return c.GSM.Validate()
}

// Deps are the ports the gateway needs.
type Deps struct {
	Clock     driver.Clock
	Store     driver.Store
	Publisher outbox.Publisher
	NewID     func() string
	Logger    *slog.Logger
	Lines     map[string]Modem // one per configured line
}

// Message is a message received from the broker.
type Message struct {
	Topic   string
	Payload []byte
}

// Subscription is a topic filter the gateway listens to.
type Subscription struct {
	Filter string
	QoS    byte
}

const (
	src        = "gsm"
	keyGSM     = "gsm"
	keySeq     = "gsm_seq"
	maxPayload = 64 * 1024
	lineQueue  = 8
)

// Gateway is the application.
type Gateway struct {
	cfg  Config
	deps Deps
	log  *slog.Logger

	core *gsm.Gateway
	loop *driver.Loop[gsm.Input, gsm.Action, gsm.Snapshot]
	out  *outbox.Outbox
	seen *outbox.RecentIDs

	workers map[string]chan func(context.Context)
	last    contract.GSMStatusData // the last status published, owned by the loop
	sent    bool

	life context.Context
	stop context.CancelFunc
	ran  atomic.Bool
}

// New builds the gateway, loading the saved queue.
func New(cfg Config, deps Deps) (*Gateway, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.Clock == nil || deps.Store == nil || deps.Publisher == nil || deps.NewID == nil {
		return nil, errors.New("gateway: Clock, Store, Publisher and NewID are required")
	}
	for _, l := range cfg.GSM.Lines {
		if deps.Lines[l.ID] == nil {
			return nil, fmt.Errorf("gateway: no modem for line %q", l.ID)
		}
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	g := &Gateway{cfg: cfg, deps: deps, log: log, seen: outbox.NewRecentIDs(cfg.DedupeSize), workers: map[string]chan func(context.Context){}}
	for _, l := range cfg.GSM.Lines {
		g.workers[l.ID] = make(chan func(context.Context), lineQueue)
	}

	var err error
	if g.core, err = gsm.New(cfg.GSM); err != nil {
		return nil, err
	}
	seq, err := outbox.NewSequencer(deps.Store, keySeq, log)
	if err != nil {
		return nil, err
	}
	g.out = outbox.New(deps.Publisher, seq, cfg.Site, cfg.OutboxSize, cfg.RetryMin, cfg.RetryMax, log)
	g.loop, err = driver.New(driver.Options[gsm.Input, gsm.Action, gsm.Snapshot]{
		Name: keyGSM, Clock: deps.Clock, Store: deps.Store, Logger: log,
		Core: driver.Core[gsm.Input, gsm.Action, gsm.Snapshot]{
			Handle: g.core.Handle, NextWake: g.core.NextWake, Snapshot: g.core.Snapshot, Restore: g.core.Restore,
			Tick: func(t time.Time) gsm.Input { return gsm.Tick{Now: t} },
		},
		Dispatch: g.dispatch,
		OnChange: func(_ context.Context, now time.Time) { g.publishStatus(now) },
	})
	if err != nil {
		return nil, err
	}
	g.life, g.stop = context.WithCancel(context.Background())
	return g, nil
}

// Subscriptions lists what the gateway listens to.
func (g *Gateway) Subscriptions() []Subscription {
	return []Subscription{{Filter: contract.AlertRequestTopic(g.cfg.Site), QoS: 1}}
}

// Alive returns once the core's loop has taken what was queued for it: it is not
// stuck. The program checks it before telling systemd that it still works (the
// watchdog).
func (g *Gateway) Alive(ctx context.Context) error { return g.loop.Alive(ctx) }

// Run serves the gateway until ctx is cancelled. It must be called once.
func (g *Gateway) Run(ctx context.Context) error {
	if !g.ran.CompareAndSwap(false, true) {
		return errors.New("gateway: Run called twice")
	}
	defer g.stop()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	start := func(f func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); f(runCtx) }()
	}
	start(func(c context.Context) { g.loop.Run(c) })
	start(g.out.Run)
	for _, id := range slices.Sorted(maps.Keys(g.workers)) {
		ch := g.workers[id]
		start(func(c context.Context) { worker(c, ch) })
	}
	// The first status, even if nothing changed since the last run.
	g.repeatStatus(runCtx)
	start(g.statusRepeater)
	<-runCtx.Done()
	cancel()
	wg.Wait()
	g.out.Flush(3 * time.Second)
	return nil
}

// statusRepeater sends the status again every StatusEvery. It goes through the
// domain's loop, so a loop that hangs stops the repetition and the hub notices.
func (g *Gateway) statusRepeater(ctx context.Context) {
	for {
		fired, stop := g.deps.Clock.Timer(g.cfg.StatusEvery)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-fired:
			g.repeatStatus(ctx)
		}
	}
}

func (g *Gateway) repeatStatus(ctx context.Context) {
	g.loop.Do(ctx, func(now time.Time) []gsm.Action {
		g.sent = false
		g.publishStatus(now)
		return nil
	})
}

// --- inbound -----------------------------------------------------------------------------

// HandleMessage takes a message from the broker. It returns quickly. Payloads are
// never logged: they hold phone texts.
func (g *Gateway) HandleMessage(m Message) {
	if len(m.Payload) > maxPayload || m.Topic != contract.AlertRequestTopic(g.cfg.Site) {
		return
	}
	env, err := contract.ParseEnvelope(m.Payload)
	if err != nil || env.Site != g.cfg.Site || env.Type != contract.TypeAlertRequest {
		g.log.Warn("ignoring an invalid alert request", "err", err)
		return
	}
	if !g.seen.FirstTime(env.ID) {
		return
	}
	d, err := contract.DecodeData[contract.AlertRequestData](env)
	if err != nil {
		g.log.Warn("ignoring an alert request with a damaged payload", "id", env.ID)
		return
	}
	r, ok := gsmwire.Request(d, env.ID, time.Time{})
	if !ok {
		return // for another channel
	}
	g.submit(func(now time.Time) gsm.Input { r.Now = now; return r })
}

// Incoming is an SMS a modem received. The composition root wires the modems'
// notifications to it.
func (g *Gateway) Incoming(line, from, text string) {
	g.submit(func(now time.Time) gsm.Input { return gsm.Incoming{Now: now, Line: line, From: from, Text: text} })
}

// Delivered is an operator's delivery report for the SMS with reference ref.
func (g *Gateway) Delivered(line, ref string, ok bool) {
	g.submit(func(now time.Time) gsm.Input { return gsm.DeliveryReport{Now: now, Line: line, MsgRef: ref, OK: ok} })
}

func (g *Gateway) submit(build func(time.Time) gsm.Input) {
	if err := g.loop.Submit(g.life, build); err != nil && g.life.Err() == nil {
		g.log.Error("handing an input to the gateway failed", "err", err)
	}
}

// --- outbound ----------------------------------------------------------------------------

// dispatch runs on the loop's goroutine: it hands modem work to the lines' workers
// and queues the messages, without ever waiting for a modem.
func (g *Gateway) dispatch(_ context.Context, now time.Time, actions []gsm.Action) error {
	for _, a := range actions {
		switch v := a.(type) {
		case gsm.Execute:
			g.toLine(v.Line, func(ctx context.Context) { g.execute(ctx, v) })
		case gsm.CheckLine:
			g.toLine(v.Line, func(ctx context.Context) { g.check(ctx, v.Line) })
		case gsm.RestartModem:
			g.log.Warn("the modem does not answer, restarting it", "line", v.Line)
			g.toLine(v.Line, func(ctx context.Context) { g.restart(ctx, v.Line) })
		case gsm.Report:
			g.enqueue(contract.AlertResultTopic(g.cfg.Site), contract.TypeAlertResult, gsmwire.Result(v), false, now)
		case gsm.Acknowledge:
			g.log.Info("a contact acknowledged by SMS", "incident", v.Incident, "contact", v.Contact)
			g.enqueue(contract.AlertAckTopic(g.cfg.Site), contract.TypeAlertAck, gsmwire.Ack(v), false, now)
		case gsm.Ignored:
			g.log.Info("an incoming SMS was ignored", "line", v.Line, "why", v.Why)
		case gsm.LinesChanged:
			// Published by publishStatus, with the queue length.
		}
	}
	return nil
}

// publishStatus publishes the lines and the queue, retained, when they changed (or
// when sent is cleared, to repeat them). It runs on the loop's goroutine.
func (g *Gateway) publishStatus(now time.Time) {
	st := gsmwire.Status(g.core.Lines(), g.core.Queued())
	if g.sent && reflect.DeepEqual(st, g.last) {
		return
	}
	g.last, g.sent = st, true
	g.enqueue(contract.GSMStatusTopic(g.cfg.Site), contract.TypeGSMStatus, st, true, now)
}

func (g *Gateway) enqueue(topic, typ string, data any, retain bool, ts time.Time) {
	raw, err := json.Marshal(data)
	if err != nil {
		g.log.Error("encoding a message failed", "type", typ, "err", err)
		return
	}
	g.out.Put(&outbox.Item{Topic: topic, Src: src, Type: typ, Data: raw, QoS: 1, Retain: retain, ID: g.deps.NewID(), TS: ts})
}
