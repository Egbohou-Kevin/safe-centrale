// Package agentapp is the application layer of the link agent: it reads the hub's
// broker and the cloud's broker, feeds the agent core (internal/agent), sends its
// queue to the cloud, and publishes on the hub what the cloud may send.
//
// It knows two publishers (the hub's broker, the cloud's broker), the queue's store,
// the photos' folder and an uploader, and no adapter: the composition root
// (cmd/safehome-agent) wires the two MQTT clients, the disk and HTTPS to it.
package agentapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"safehome/internal/agent"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/outbox"
)

// Config holds what the agent needs to know.
type Config struct {
	Site  string
	Relay agent.Config
	// StatusEvery is how often agent.status goes out even when nothing changed: the
	// hub takes an agent that stopped repeating it for dead, and the internet for
	// down.
	StatusEvery time.Duration
	OutboxSize  int
	RetryMin    time.Duration // between two attempts to send to the cloud, at first
	RetryMax    time.Duration // and at most
	SendTimeout time.Duration // for the cloud's broker to acknowledge one message
	PhotoWait   time.Duration // for one photo's upload
}

// DefaultConfig returns the defaults.
func DefaultConfig(site string) Config {
	return Config{
		Site: site, Relay: agent.DefaultConfig(site), StatusEvery: time.Minute, OutboxSize: 1000,
		RetryMin: time.Second, RetryMax: 30 * time.Second, SendTimeout: 10 * time.Second, PhotoWait: time.Minute,
	}
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if c.Relay.Site != c.Site {
		return fmt.Errorf("agentapp: the relay's site %q differs from %q", c.Relay.Site, c.Site)
	}
	if c.StatusEvery <= 0 || c.OutboxSize < 1 || c.RetryMin <= 0 || c.RetryMax < c.RetryMin || c.SendTimeout <= 0 || c.PhotoWait <= 0 {
		return errors.New("agentapp: StatusEvery, OutboxSize, RetryMin, RetryMax, SendTimeout and PhotoWait must be set")
	}
	return c.Relay.Validate()
}

// Deps are the ports the agent needs.
type Deps struct {
	Clock    driver.Clock
	Store    driver.Store     // the core's snapshot and the agent's counter
	Queue    QueueStore       // the core's queue for the cloud
	Local    outbox.Publisher // the hub's broker
	Cloud    outbox.Publisher // the cloud's broker; ErrUnavailable while it cannot be reached
	Photos   Photos
	Uploader Uploader
	NewID    func() string
	// Logger: never given a message's content, which may hold a video token.
	Logger *slog.Logger
}

// Message is a message received from one of the brokers.
type Message struct {
	Topic   string
	Payload []byte
}

// Subscription is a topic filter the agent listens to. NoLocal is always wanted:
// the agent republishes between two brokers and must not hear itself.
type Subscription struct {
	Filter  string
	QoS     byte
	NoLocal bool
}

const (
	src        = contract.AgentLink
	keyAgent   = "agent"
	keySeq     = "agent_seq"
	maxPayload = 256 * 1024
)

// App is the application.
type App struct {
	cfg  Config
	deps Deps
	log  *slog.Logger

	core   *agent.Relay
	loop   *driver.Loop[agent.Input, agent.Action, agent.Snapshot]
	out    *outbox.Outbox
	seq    *outbox.Sequencer
	kick   chan struct{} // the queue moved: something may be ready for the cloud
	relink chan struct{} // the link changed: worth trying again at once

	uploads sync.WaitGroup // the photos being sent

	life context.Context
	stop context.CancelFunc
	ran  atomic.Bool
}

// New builds the agent, loading its saved queue.
func New(cfg Config, deps Deps) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.Clock == nil || deps.Store == nil || deps.Queue == nil || deps.Local == nil || deps.Cloud == nil ||
		deps.Photos == nil || deps.Uploader == nil || deps.NewID == nil {
		return nil, errors.New("agentapp: Clock, Store, Queue, Local, Cloud, Photos, Uploader and NewID are required")
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	core, err := agent.New(cfg.Relay)
	if err != nil {
		return nil, err
	}
	seq, err := outbox.NewSequencer(deps.Store, keySeq, log)
	if err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, deps: deps, log: log, core: core, seq: seq, kick: make(chan struct{}, 1), relink: make(chan struct{}, 1)}
	a.life, a.stop = context.WithCancel(context.Background())
	a.out = outbox.New(deps.Local, seq, cfg.Site, cfg.OutboxSize, 200*time.Millisecond, 5*time.Second, log)
	a.loop, err = driver.New(driver.Options[agent.Input, agent.Action, agent.Snapshot]{
		Name: keyAgent, Clock: deps.Clock, Store: deps.Store, Logger: log,
		Core: driver.Core[agent.Input, agent.Action, agent.Snapshot]{
			Handle: core.Handle, NextWake: core.NextWake, Snapshot: core.Snapshot, Restore: core.Restore,
			Tick: func(t time.Time) agent.Input { return agent.Tick{Now: t} },
		},
		Dispatch: a.dispatch,
		OnChange: func(context.Context, time.Time) { a.poke() }, // the queue moved
	})
	if err != nil {
		return nil, err
	}
	if err := a.restoreQueue(); err != nil {
		return nil, err
	}
	return a, nil
}

// LocalSubscriptions lists what the agent reads on the hub's broker.
func (a *App) LocalSubscriptions() []Subscription { return subscriptions(agent.UpFilters(a.cfg.Site)) }

// CloudSubscriptions lists what the agent reads on the cloud's broker.
func (a *App) CloudSubscriptions() []Subscription {
	return subscriptions(agent.DownFilters(a.cfg.Site))
}

func subscriptions(filters []string) []Subscription {
	out := make([]Subscription, len(filters))
	for i, f := range filters {
		out[i] = Subscription{Filter: f, QoS: 1, NoLocal: true}
	}
	return out
}

// Alive returns once the core's loop has taken what was queued for it: it is not
// stuck. The program checks it before telling systemd that it still works (the
// watchdog).
func (a *App) Alive(ctx context.Context) error { return a.loop.Alive(ctx) }

// Run serves the agent until ctx is cancelled. It must be called once.
func (a *App) Run(ctx context.Context) error {
	if !a.ran.CompareAndSwap(false, true) {
		return errors.New("agentapp: Run called twice")
	}
	defer a.stop()
	var workers sync.WaitGroup
	workers.Go(func() { a.loop.Run(ctx) })
	workers.Go(func() { a.out.Run(ctx) })
	// The status at start: the link is down until the cloud's broker answers.
	a.loop.Do(ctx, func(now time.Time) []agent.Action {
		a.publishStatus(now, a.core.Status(now))
		return nil
	})
	workers.Go(func() { a.statusRepeater(ctx) })
	workers.Go(func() { a.sender(ctx) })
	<-ctx.Done()
	a.stop() // an upload under way gives up; it is tried again at the next start
	workers.Wait()
	a.uploads.Wait()
	a.out.Flush(3 * time.Second)
	return nil
}

// HandleLocal takes a message from the hub's broker. It returns at once.
func (a *App) HandleLocal(m Message) {
	if len(m.Payload) > maxPayload {
		a.log.Warn("not sending an oversized message to the cloud", "topic", m.Topic, "bytes", len(m.Payload))
		return
	}
	a.submit(func(now time.Time) agent.Input { return agent.Local{Now: now, Topic: m.Topic, Payload: m.Payload} })
}

// HandleCloud takes a message from the cloud's broker. It returns at once.
func (a *App) HandleCloud(m Message) {
	if len(m.Payload) > maxPayload {
		a.log.Warn("refusing an oversized message from the cloud", "topic", m.Topic, "bytes", len(m.Payload))
		return
	}
	a.submit(func(now time.Time) agent.Input { return agent.Cloud{Now: now, Topic: m.Topic, Payload: m.Payload} })
}

// LinkChanged is told when the connection to the cloud comes up or goes down, with
// the URL scheme in use.
func (a *App) LinkChanged(up bool, scheme string) {
	a.submit(func(now time.Time) agent.Input { return agent.Link{Now: now, Up: up, Transport: scheme} })
}

// Will is the message the cloud's broker publishes if the agent disappears without a
// word: agent.status down, retained, so the cloud knows at once that the site is
// offline. Its seq is 0: it is not the agent's to number.
func (a *App) Will() (topic string, payload []byte) {
	env, err := contract.NewEnvelope(a.cfg.Site, src, 0, a.deps.NewID(), a.deps.Clock.Now(), contract.TypeAgentStatus,
		contract.AgentStatusData{Uplink: contract.UplinkDown})
	if err != nil {
		panic(err) // a constant message: cannot fail
	}
	b, _ := json.Marshal(env)
	return contract.AgentStatusTopic(a.cfg.Site), b
}

func (a *App) submit(build func(now time.Time) agent.Input) {
	err := a.loop.Submit(a.life, build)
	if err != nil && !errors.Is(err, driver.ErrStopped) && !errors.Is(err, context.Canceled) {
		a.log.Error("the agent's loop refused an input", "err", err)
	}
}
