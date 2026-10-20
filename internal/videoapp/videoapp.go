// Package videoapp is the application layer of the site's video service: it feeds
// the video core with the cloud's requests (video/cmd), starts and stops the streams
// it decides, tells it how many bytes they sent, and publishes video.session and
// video.status. When the alarm goes off (alarm.triggered) it takes the photos the
// core decides, keeps them for the link agent and announces them (photo.taken).
//
// The streams are behind the Streamer port, declared here and wired in the
// composition root (cmd/safehome-video) to go2rtc, so the application is tested with
// a fake. Video never takes part in the alarm: nothing here waits for the engine,
// and the engine waits for nothing here.
package videoapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/outbox"
	"safehome/internal/video"
	videowire "safehome/internal/video/wire"
)

// Streamer pushes the cameras' streams.
type Streamer interface {
	// Start pushes the camera's stream to target (credential included: never log
	// it) and returns once the camera is connected. An error wrapping
	// ErrCameraUnreachable says the camera did not answer; any other error counts
	// as the relay's.
	Start(ctx context.Context, camera, target string) (Stream, error)
	// Probe says whether the camera answers on the local network.
	Probe(ctx context.Context, camera string) error
}

// Stream is a stream under way.
type Stream interface {
	// Sent returns the bytes sent so far, or an error once the stream is dead.
	Sent(ctx context.Context) (int64, error)
	// Stop ends the stream and frees what it holds.
	Stop()
}

// Photographer takes the pictures of an alarm.
type Photographer interface {
	// Open readies every camera for pictures. The Shooter is closed once they are
	// taken.
	Open(ctx context.Context) (Shooter, error)
}

// Shooter takes pictures.
type Shooter interface {
	// Frame returns a JPEG picture of the camera, width pixels wide.
	Frame(ctx context.Context, camera string, width int) ([]byte, error)
	Close()
}

// PhotoStore keeps the photos until the link agent has sent them.
type PhotoStore interface {
	Save(id string, jpeg []byte) error
}

// ErrCameraUnreachable says the camera did not answer.
var ErrCameraUnreachable = errors.New("videoapp: camera unreachable")

// Config holds what the service needs to know.
type Config struct {
	Site          string
	Video         video.Config
	OutboxSize    int
	RetryMin      time.Duration
	RetryMax      time.Duration
	DedupeSize    int
	StatusEvery   time.Duration // video.status goes out again even when nothing changed
	ProgressEvery time.Duration // how often a stream's bytes are read
	StartWait     time.Duration // for the camera to answer when a stream starts
	ProbeWait     time.Duration // for a camera to answer a probe
	PhotoWait     time.Duration // for the cameras to be ready, then for each picture
}

// DefaultConfig returns the defaults.
func DefaultConfig(site string, v video.Config) Config {
	return Config{
		Site: site, Video: v, OutboxSize: 200, RetryMin: 200 * time.Millisecond, RetryMax: 5 * time.Second,
		DedupeSize: 256, StatusEvery: time.Minute, ProgressEvery: 2 * time.Second,
		StartWait: 15 * time.Second, ProbeWait: 3 * time.Second, PhotoWait: 10 * time.Second,
	}
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if !contract.ValidSite(c.Site) {
		return fmt.Errorf("videoapp: invalid site %q", c.Site)
	}
	if c.OutboxSize < 1 || c.RetryMin <= 0 || c.RetryMax < c.RetryMin || c.DedupeSize < 1 ||
		c.StatusEvery <= 0 || c.ProgressEvery <= 0 || c.StartWait <= 0 || c.ProbeWait <= 0 || c.PhotoWait <= 0 {
		return errors.New("videoapp: OutboxSize, RetryMin, RetryMax, DedupeSize, StatusEvery, ProgressEvery, StartWait, ProbeWait and PhotoWait must be set")
	}
	return c.Video.Validate()
}

// Deps are the ports the service needs.
type Deps struct {
	Clock     driver.Clock
	Store     driver.Store
	Publisher outbox.Publisher
	Streamer  Streamer
	Camera    Photographer
	Photos    PhotoStore
	NewID     func() string
	// Logger: never given a request's target nor token.
	Logger *slog.Logger
}

// Message is a message received from the broker.
type Message struct {
	Topic   string
	Payload []byte
}

// Subscription is a topic filter the service listens to.
type Subscription struct {
	Filter string
	QoS    byte
}

const (
	src        = "video"
	keyVideo   = "video"
	keySeq     = "video_seq"
	maxPayload = 16 * 1024
)

// running is a session whose stream the application drives.
type running struct {
	cancel context.CancelFunc
}

// App is the application.
type App struct {
	cfg  Config
	deps Deps
	log  *slog.Logger

	core *video.Service
	loop *driver.Loop[video.Input, video.Action, video.Snapshot]
	out  *outbox.Outbox
	seen *outbox.RecentIDs

	mu       sync.Mutex
	base     context.Context     // the streams' parent, set by Run
	sessions map[string]*running // owned by the loop's goroutine, read at the end
	streams  sync.WaitGroup      // the streams' goroutines

	last contract.VideoStatusData // the last status published, owned by the loop
	sent bool

	life context.Context
	stop context.CancelFunc
	ran  atomic.Bool
}

// New builds the service, loading the month's usage.
func New(cfg Config, deps Deps) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.Clock == nil || deps.Store == nil || deps.Publisher == nil || deps.Streamer == nil || deps.Camera == nil ||
		deps.Photos == nil || deps.NewID == nil {
		return nil, errors.New("videoapp: Clock, Store, Publisher, Streamer, Camera, Photos and NewID are required")
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	a := &App{cfg: cfg, deps: deps, log: log, seen: outbox.NewRecentIDs(cfg.DedupeSize), sessions: map[string]*running{}}
	a.life, a.stop = context.WithCancel(context.Background())
	var err error
	if a.core, err = video.New(cfg.Video); err != nil {
		return nil, err
	}
	seq, err := outbox.NewSequencer(deps.Store, keySeq, log)
	if err != nil {
		return nil, err
	}
	a.out = outbox.New(deps.Publisher, seq, cfg.Site, cfg.OutboxSize, cfg.RetryMin, cfg.RetryMax, log)
	a.loop, err = driver.New(driver.Options[video.Input, video.Action, video.Snapshot]{
		Name: keyVideo, Clock: deps.Clock, Store: deps.Store, Logger: log,
		Core: driver.Core[video.Input, video.Action, video.Snapshot]{
			Handle: a.core.Handle, NextWake: a.core.NextWake, Snapshot: a.core.Snapshot, Restore: a.core.Restore,
			Tick: func(t time.Time) video.Input { return video.Tick{Now: t} },
		},
		Dispatch: a.dispatch,
		OnChange: func(_ context.Context, now time.Time) { a.publishStatus(now) },
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// Subscriptions lists what the service listens to: the cloud's orders, and the
// alarm's events, for photos.
func (a *App) Subscriptions() []Subscription {
	return []Subscription{{Filter: contract.VideoCmdTopic(a.cfg.Site), QoS: 1}, {Filter: contract.AlarmEventTopic(a.cfg.Site), QoS: 1}}
}

// Alive returns once the core's loop has taken what was queued for it: it is not
// stuck. The program checks it before telling systemd that it still works (the
// watchdog).
func (a *App) Alive(ctx context.Context) error { return a.loop.Alive(ctx) }

// Run serves the service until ctx is cancelled. It must be called once. At the end
// every stream is stopped.
func (a *App) Run(ctx context.Context) error {
	if !a.ran.CompareAndSwap(false, true) {
		return errors.New("videoapp: Run called twice")
	}
	defer a.stop()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.base = runCtx
	a.mu.Unlock()
	var wg sync.WaitGroup
	start := func(f func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); f(runCtx) }()
	}
	start(func(c context.Context) { a.loop.Run(c) })
	start(a.out.Run)
	a.repeatStatus(runCtx)
	start(a.statusRepeater)
	<-runCtx.Done()
	cancel()
	wg.Wait()
	a.streams.Wait() // every stream stopped
	a.out.Flush(3 * time.Second)
	return nil
}

func (a *App) statusRepeater(ctx context.Context) {
	for {
		fired, stop := a.deps.Clock.Timer(a.cfg.StatusEvery)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-fired:
			a.repeatStatus(ctx)
		}
	}
}

func (a *App) repeatStatus(ctx context.Context) {
	a.loop.Do(ctx, func(now time.Time) []video.Action {
		a.sent = false
		a.publishStatus(now)
		return nil
	})
}

// --- inbound -----------------------------------------------------------------------

// HandleMessage takes a message from the broker. It returns quickly. Payloads are
// never logged: a request holds the relay's token.
func (a *App) HandleMessage(m Message) {
	if len(m.Payload) > maxPayload {
		return
	}
	if m.Topic == contract.AlarmEventTopic(a.cfg.Site) {
		a.alarmEvent(m.Payload)
		return
	}
	if m.Topic != contract.VideoCmdTopic(a.cfg.Site) {
		return
	}
	env, err := contract.ParseEnvelope(m.Payload)
	if err != nil || env.Site != a.cfg.Site {
		a.log.Warn("ignoring an invalid video order", "err", err)
		return
	}
	if !a.seen.FirstTime(env.ID) {
		return
	}
	switch env.Type {
	case contract.TypeVideoRequest:
		d, err := contract.DecodeData[contract.VideoRequestData](env)
		if err == nil {
			err = d.Validate()
		}
		if err != nil {
			a.log.Warn("ignoring an invalid video request", "id", env.ID, "err", err)
			return
		}
		a.log.Info("video requested", "request", d) // d masks its token
		a.submit(func(now time.Time) video.Input { return videowire.Request(d, now) })
	case contract.TypeVideoStop:
		d, err := contract.DecodeData[contract.VideoStopData](env)
		if err == nil {
			err = d.Validate()
		}
		if err != nil {
			a.log.Warn("ignoring an invalid video stop", "id", env.ID, "err", err)
			return
		}
		a.submit(func(now time.Time) video.Input { return videowire.Stop(d, now) })
	default:
		a.log.Warn("ignoring an unknown video order", "type", env.Type)
	}
}

// alarmEvent takes the alarm's events: only alarm.triggered matters, for photos.
func (a *App) alarmEvent(payload []byte) {
	env, err := contract.ParseEnvelope(payload)
	if err != nil || env.Site != a.cfg.Site || env.Type != contract.TypeAlarmTriggered || !a.seen.FirstTime(env.ID) {
		return
	}
	d, err := contract.DecodeData[contract.TriggeredData](env)
	if err != nil || d.Incident == "" {
		a.log.Warn("ignoring an invalid alarm.triggered", "err", err)
		return
	}
	a.submit(func(now time.Time) video.Input { return videowire.Alarm(d, now) })
}

func (a *App) submit(build func(time.Time) video.Input) {
	if err := a.loop.Submit(a.life, build); err != nil && a.life.Err() == nil {
		a.log.Error("handing an input to the video service failed", "err", err)
	}
}

// --- outbound ----------------------------------------------------------------------

// dispatch runs on the loop's goroutine: it starts and stops streams in their own
// goroutines, and queues the messages, without ever waiting for a camera.
func (a *App) dispatch(_ context.Context, now time.Time, actions []video.Action) error {
	for _, act := range actions {
		switch v := act.(type) {
		case video.Start:
			a.startStream(v)
		case video.Halt:
			if r, ok := a.sessions[v.Session]; ok {
				r.cancel()
				delete(a.sessions, v.Session)
			}
		case video.Report:
			a.log.Info("video session", "session", v.Session, "camera", v.Camera, "status", v.Status, "reason", v.Reason, "bytes", v.Bytes)
			a.enqueue(contract.VideoSessionTopic(a.cfg.Site), contract.TypeVideoSession, videowire.Session(v), false, now)
		case video.CheckCameras:
			a.checkCameras()
		case video.TakePhotos:
			a.takePhotos(v)
		case video.NoPhotos:
			a.log.Warn("no photos for this alarm", "incident", v.Incident, "why", v.Why)
		}
	}
	a.publishStatus(now) // sessions started or ended
	return nil
}

func (a *App) startStream(s video.Start) {
	a.mu.Lock()
	base := a.base
	a.mu.Unlock()
	if base == nil || base.Err() != nil {
		return // stopping: the session will not start, and nothing is pushed
	}
	ctx, cancel := context.WithCancel(base)
	a.sessions[s.Session] = &running{cancel: cancel}
	a.streams.Add(1)
	go func() {
		defer a.streams.Done()
		defer cancel()
		a.stream(ctx, s)
	}()
}

// stream starts one session's stream, reads its bytes until the core halts it, then
// stops it.
func (a *App) stream(ctx context.Context, s video.Start) {
	startCtx, cancel := context.WithTimeout(ctx, a.cfg.StartWait)
	st, err := a.deps.Streamer.Start(startCtx, s.Camera, s.Target)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return // halted meanwhile
		}
		why := video.RelayUnreachable
		if errors.Is(err, ErrCameraUnreachable) {
			why = video.CameraUnreachable
		}
		a.log.Warn("a video stream could not start", "session", s.Session, "camera", s.Camera, "err", err)
		a.submit(func(now time.Time) video.Input { return video.StreamFailed{Now: now, Session: s.Session, Reason: why} })
		return
	}
	defer st.Stop()
	for {
		fired, stop := a.deps.Clock.Timer(a.cfg.ProgressEvery)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-fired:
		}
		n, err := st.Sent(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			a.log.Warn("a video stream broke", "session", s.Session, "err", err)
			a.submit(func(now time.Time) video.Input {
				return video.StreamFailed{Now: now, Session: s.Session, Reason: video.RelayUnreachable}
			})
			return
		}
		a.submit(func(now time.Time) video.Input { return video.Progress{Now: now, Session: s.Session, Bytes: n} })
	}
}

// checkCameras probes every camera, one after the other, away from the loop.
func (a *App) checkCameras() {
	a.mu.Lock()
	base := a.base
	a.mu.Unlock()
	if base == nil || base.Err() != nil {
		return
	}
	a.streams.Add(1)
	go func() {
		defer a.streams.Done()
		for _, c := range a.cfg.Video.Cameras {
			ctx, cancel := context.WithTimeout(base, a.cfg.ProbeWait)
			err := a.deps.Streamer.Probe(ctx, c.ID)
			cancel()
			if base.Err() != nil {
				return
			}
			if err != nil {
				a.log.Warn("a camera does not answer", "camera", c.ID, "err", err)
			}
			a.submit(func(now time.Time) video.Input { return video.Probe{Now: now, Camera: c.ID, Reachable: err == nil} })
		}
	}()
}

// takePhotos takes an alarm's photos away from the loop: Count rounds Every apart,
// one picture of each camera per round. A camera that gives no picture is left out of
// the next rounds. Each photo is kept, then announced.
func (a *App) takePhotos(tp video.TakePhotos) {
	a.mu.Lock()
	base := a.base
	a.mu.Unlock()
	if base == nil || base.Err() != nil {
		return
	}
	a.streams.Add(1)
	go func() {
		defer a.streams.Done()
		ctx, cancel := context.WithTimeout(base, a.cfg.PhotoWait)
		sh, err := a.deps.Camera.Open(ctx)
		cancel()
		if err != nil {
			a.log.Warn("the photos of the alarm could not be taken", "incident", tp.Incident, "err", err)
			return
		}
		defer sh.Close()
		var taken int
		var bytes int64
		failed := map[string]bool{}
		for round := range tp.Count {
			if round > 0 {
				fired, stop := a.deps.Clock.Timer(tp.Every)
				select {
				case <-base.Done():
					stop()
					return
				case <-fired:
				}
			}
			for _, cam := range tp.Cameras {
				if failed[cam] || base.Err() != nil {
					continue
				}
				ctx, cancel := context.WithTimeout(base, a.cfg.PhotoWait)
				jpeg, err := sh.Frame(ctx, cam, tp.Width)
				cancel()
				if err != nil {
					a.log.Warn("a camera gave no photo", "incident", tp.Incident, "camera", cam, "err", err)
					failed[cam] = true
					continue
				}
				id := a.deps.NewID()
				if err := a.deps.Photos.Save(id, jpeg); err != nil {
					a.log.Error("a photo could not be kept", "incident", tp.Incident, "camera", cam, "err", err)
					continue
				}
				taken++
				bytes += int64(len(jpeg))
				a.enqueue(contract.PhotoTakenTopic(a.cfg.Site), contract.TypePhotoTaken, contract.PhotoTakenData{
					Photo: id, Incident: tp.Incident, Camera: cam, Bytes: int64(len(jpeg)), Width: tp.Width,
				}, false, a.deps.Clock.Now())
			}
		}
		a.log.Info("photos of the alarm taken", "incident", tp.Incident, "photos", taken, "bytes", bytes)
		if bytes > 0 {
			a.submit(func(now time.Time) video.Input { return video.PhotosTaken{Now: now, Bytes: bytes} })
		}
	}()
}

// publishStatus publishes video.status, retained, when it changed (or when sent is
// cleared, to repeat it). It runs on the loop's goroutine.
func (a *App) publishStatus(now time.Time) {
	st := videowire.Status(a.core.State())
	if a.sent && reflect.DeepEqual(st, a.last) {
		return
	}
	a.last, a.sent = st, true
	a.enqueue(contract.VideoStatusTopic(a.cfg.Site), contract.TypeVideoStatus, st, true, now)
}

func (a *App) enqueue(topic, typ string, data any, retain bool, ts time.Time) {
	raw, err := json.Marshal(data)
	if err != nil {
		a.log.Error("encoding a message failed", "type", typ, "err", err)
		return
	}
	a.out.Put(&outbox.Item{Topic: topic, Src: src, Type: typ, Data: raw, QoS: 1, Retain: retain, ID: a.deps.NewID(), TS: ts})
}
