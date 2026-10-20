// Package agentapp is the application layer of the link agent: it reads the hub's
// broker and the cloud's broker, feeds the agent core (internal/agent), sends its
// queue to the cloud, and publishes on the hub what the cloud may send.
//
// It knows two publishers (the hub's broker, the cloud's broker), the photos' folder
// and an uploader, and no adapter: the composition root (cmd/safehome-agent) wires
// the two MQTT clients, the folder and HTTPS to it.
package agentapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Photos is the photos' folder, which the video service fills.
type Photos interface {
	// Open returns a photo and its size; an error wrapping ErrNoPhoto says it is gone.
	Open(id string) (io.ReadCloser, int64, error)
	Remove(id string) error
}

// Uploader sends a photo to the cloud over HTTPS.
type Uploader interface {
	// Put sends the photo to url with the token (never log it). An error wrapping
	// ErrUploadExpired says the address or the token were refused, one wrapping
	// ErrUploadRefused that the file was; any other is worth another try.
	Put(ctx context.Context, url, token string, body io.Reader, size int64) error
}

// The errors of the photos' ports.
var (
	ErrNoPhoto       = errors.New("agentapp: no such photo")
	ErrUploadExpired = errors.New("agentapp: upload address refused")
	ErrUploadRefused = errors.New("agentapp: photo refused")
)

// Deps are the ports the agent needs.
type Deps struct {
	Clock    driver.Clock
	Store    driver.Store
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
	if deps.Clock == nil || deps.Store == nil || deps.Local == nil || deps.Cloud == nil || deps.Photos == nil ||
		deps.Uploader == nil || deps.NewID == nil {
		return nil, errors.New("agentapp: Clock, Store, Local, Cloud, Photos, Uploader and NewID are required")
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
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	const workers = 4
	done := make(chan struct{}, workers)
	start := func(f func(context.Context)) { go func() { f(runCtx); done <- struct{}{} }() }
	start(func(c context.Context) { a.loop.Run(c) })
	start(a.out.Run)
	// The status at start: the link is down until the cloud's broker answers.
	a.loop.Do(runCtx, func(now time.Time) []agent.Action {
		a.publishStatus(now, a.core.Status(now))
		return nil
	})
	start(a.statusRepeater)
	start(a.sender)
	<-runCtx.Done()
	a.stop() // an upload under way gives up; it is tried again at the next start
	for range workers {
		<-done
	}
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

// --- outbound ----------------------------------------------------------------------

// dispatch carries out what the core decided. It runs on the loop's goroutine.
func (a *App) dispatch(_ context.Context, now time.Time, actions []agent.Action) error {
	for _, act := range actions {
		switch v := act.(type) {
		case agent.PublishLocal:
			a.out.Put(&outbox.Item{Topic: v.Topic, Data: v.Payload, QoS: 1, Raw: true})
		case agent.FailAlert:
			a.log.Warn("an alert for the app could not reach the cloud: the hub turns to the GSM", "ref", v.Ref)
			a.publishOwn(now, contract.AlertResultTopic(a.cfg.Site), contract.TypeAlertResult, contract.AlertResultData{
				Ref: v.Ref, Status: contract.ResultFailed, Reason: contract.ReasonNoUplink,
			}, false)
		case agent.StatusChanged:
			if v.Status.Up {
				a.log.Info("connected to the cloud", "transport", v.Status.Transport, "queued", v.Status.Queued)
			} else {
				a.log.Warn("the cloud cannot be reached", "queued", v.Status.Queued)
			}
			a.publishStatus(now, v.Status)
			signal(a.relink)
		case agent.Refused:
			a.log.Warn("refused a message from the cloud", "topic", v.Topic, "reason", v.Reason)
		case agent.Dropped:
			a.log.Error("messages for the cloud were dropped", "count", v.Count, "reason", v.Reason)
		case agent.Replayed:
			a.log.Info("the cloud asked for messages again", "from", v.Report.From, "to", v.Report.To,
				"resent", v.Report.Resent, "missing_ranges", len(v.Report.Missing))
			a.publishOwn(now, contract.AgentReplayedTopic(a.cfg.Site), contract.TypeAgentReplayed, v.Report, false)
		case agent.Upload:
			a.upload(v)
		case agent.RemovePhoto:
			if err := a.deps.Photos.Remove(v.Photo); err != nil {
				a.log.Warn("a photo could not be removed", "photo", v.Photo, "err", err)
			}
		case agent.PhotoReport:
			if v.Status == contract.PhotoSent {
				a.log.Info("photo sent to the cloud", "photo", v.Photo)
			} else {
				a.log.Warn("photo not sent", "photo", v.Photo, "reason", v.Reason)
			}
			a.publishOwn(now, contract.PhotoSentTopic(a.cfg.Site), contract.TypePhotoSent,
				contract.PhotoSentData{Photo: v.Photo, Status: v.Status, Reason: v.Reason}, false)
		}
	}
	a.poke()
	return nil
}

// upload sends a photo away from the loop, and tells the core how it went.
func (a *App) upload(u agent.Upload) {
	a.uploads.Add(1)
	go func() {
		defer a.uploads.Done()
		result := a.put(u)
		a.submit(func(now time.Time) agent.Input { return agent.Uploaded{Now: now, Photo: u.Photo, Result: result} })
	}()
}

func (a *App) put(u agent.Upload) agent.UploadResult {
	f, size, err := a.deps.Photos.Open(u.Photo)
	if errors.Is(err, ErrNoPhoto) {
		return agent.UploadMissing
	}
	if err != nil {
		a.log.Warn("a photo could not be read", "photo", u.Photo, "err", err)
		return agent.UploadRetry
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(a.life, a.cfg.PhotoWait)
	defer cancel()
	err = a.deps.Uploader.Put(ctx, u.URL, u.Token, f, size)
	switch {
	case err == nil:
		return agent.UploadOK
	case errors.Is(err, ErrUploadExpired):
		return agent.UploadExpired
	case errors.Is(err, ErrUploadRefused):
		a.log.Warn("the cloud refused a photo", "photo", u.Photo, "err", err)
		return agent.UploadRefused
	}
	if a.life.Err() == nil {
		a.log.Warn("a photo could not be sent, trying again later", "photo", u.Photo, "err", err)
	}
	return agent.UploadRetry
}

// poke wakes the sender: something may be ready for the cloud.
func (a *App) poke() { signal(a.kick) }

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// publishStatus publishes agent.status on the hub, and queues it for the cloud.
func (a *App) publishStatus(now time.Time, st agent.Status) {
	uplink := contract.UplinkDown
	if st.Up {
		uplink = contract.UplinkUp
	}
	a.publishOwn(now, contract.AgentStatusTopic(a.cfg.Site), contract.TypeAgentStatus, contract.AgentStatusData{
		Uplink: uplink, Transport: st.Transport, Queued: st.Queued, OldestS: int(st.Oldest / time.Second), Dropped: st.Dropped,
		Economy: a.cfg.Relay.Economy > 0,
	}, true)
}

// publishOwn publishes a message of the agent on the hub's broker and hands it to the
// core for the cloud: the agent does not hear its own publications (NoLocal). It runs
// on the loop's goroutine.
func (a *App) publishOwn(now time.Time, topic, typ string, data any, retain bool) {
	env, err := contract.NewEnvelope(a.cfg.Site, src, a.seq.Next(), a.deps.NewID(), now, typ, data)
	if err != nil {
		a.log.Error("building a message failed", "type", typ, "err", err)
		return
	}
	b, err := json.Marshal(env)
	if err != nil {
		a.log.Error("encoding a message failed", "type", typ, "err", err)
		return
	}
	a.out.Put(&outbox.Item{Topic: topic, Data: b, QoS: 1, Retain: retain, Raw: true})
	// On the loop: safe, and saved with this task. A full queue may answer with
	// actions of its own.
	if acts := a.core.Handle(agent.Own{Now: now, Topic: topic, Payload: b}); len(acts) > 0 {
		a.dispatch(a.life, now, acts)
	}
}

// statusRepeater sends the status again every StatusEvery, through the core's loop:
// a loop that hangs stops the repetition, and the hub notices.
func (a *App) statusRepeater(ctx context.Context) {
	for {
		fired, stop := a.deps.Clock.Timer(a.cfg.StatusEvery)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-fired:
			a.loop.Do(ctx, func(now time.Time) []agent.Action {
				a.publishStatus(now, a.core.Status(now))
				return nil
			})
		}
	}
}

// sender sends the queue to the cloud, one message at a time, alarms first. A message
// leaves the queue only once the cloud's broker acknowledged it: after a power cut,
// what was not acknowledged goes again (the cloud drops duplicates by id).
func (a *App) sender(ctx context.Context) {
	delay := a.cfg.RetryMin
	for {
		var it agent.Item
		var ok bool
		if err := a.loop.Do(ctx, func(time.Time) []agent.Action {
			if a.core.Up() {
				it, ok = a.core.Next()
			}
			return nil
		}); err != nil {
			return
		}
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-a.kick:
			case <-a.relink:
			}
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, a.cfg.SendTimeout)
		err := a.deps.Cloud.Publish(sendCtx, it.Topic, it.Payload, 1, it.Retain)
		cancel()
		if err == nil {
			delay = a.cfg.RetryMin
			a.submit(func(now time.Time) agent.Input { return agent.Sent{Now: now, ID: it.ID} })
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if !errors.Is(err, outbox.ErrUnavailable) {
			a.log.Warn("the cloud did not take a message, trying again", "topic", it.Topic, "err", err)
		}
		if !a.wait(ctx, delay) {
			return
		}
		delay = min(delay*2, a.cfg.RetryMax)
	}
}

// wait waits d on the agent's clock, or less if the link changes. New messages do
// not cut it short: the cloud's broker refused the last one, and must not be pressed
// at every new message.
func (a *App) wait(ctx context.Context, d time.Duration) bool {
	fired, stop := a.deps.Clock.Timer(d)
	defer stop()
	select {
	case <-ctx.Done():
		return false
	case <-fired:
	case <-a.relink:
	}
	return true
}
