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
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/inbox"
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
	Outbox        outbox.Settings
	DedupeSize    int
	ProgressEvery time.Duration // how often a stream's bytes are read
	StartWait     time.Duration // for the camera to answer when a stream starts
	ProbeWait     time.Duration // for a camera to answer a probe
	PhotoWait     time.Duration // for the cameras to be ready, then for each picture
}

// DefaultConfig returns the defaults.
func DefaultConfig(site string, v video.Config) Config {
	return Config{
		Site: site, Video: v, Outbox: outbox.DefaultSettings(200), DedupeSize: 256, ProgressEvery: 2 * time.Second,
		StartWait: 15 * time.Second, ProbeWait: 3 * time.Second, PhotoWait: 10 * time.Second,
	}
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if !contract.ValidSite(c.Site) {
		return fmt.Errorf("videoapp: invalid site %q", c.Site)
	}
	if c.DedupeSize < 1 || c.ProgressEvery <= 0 || c.StartWait <= 0 || c.ProbeWait <= 0 || c.PhotoWait <= 0 {
		return errors.New("videoapp: DedupeSize, ProgressEvery, StartWait, ProbeWait and PhotoWait must be set")
	}
	if err := c.Outbox.Validate(); err != nil {
		return fmt.Errorf("videoapp: %w", err)
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

const (
	src        = "video"
	keyVideo   = "video"
	keySeq     = "video_seq"
	maxPayload = 16 * 1024
)

// App is the application.
type App struct {
	cfg  Config
	deps Deps
	log  *slog.Logger

	core *video.Service
	loop *driver.Loop[video.Input, video.Action, video.Snapshot]
	out  *outbox.Outbox
	seen *inbox.RecentIDs

	sessions map[string]context.CancelFunc // the streams under way; owned by the loop's goroutine
	work     sync.WaitGroup                // the goroutines working away from the loop (streams, probes, photos)

	last contract.VideoStatusData // the last status published, owned by the loop
	sent bool                     // last is set

	// life ends with Run: the loop and the work away from it run on it, and work
	// handed to the loop after it ended is given up quietly.
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
	a := &App{cfg: cfg, deps: deps, log: log, seen: inbox.NewRecentIDs(cfg.DedupeSize), sessions: map[string]context.CancelFunc{}}
	a.life, a.stop = context.WithCancel(context.Background())
	var err error
	if a.core, err = video.New(cfg.Video); err != nil {
		return nil, err
	}
	if a.out, err = outbox.Open(deps.Store, deps.Publisher, keySeq, cfg.Site, cfg.Outbox, deps.NewID, log); err != nil {
		return nil, err
	}
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
func (a *App) Subscriptions() []inbox.Subscription {
	return []inbox.Subscription{{Filter: contract.VideoCmdTopic(a.cfg.Site), QoS: 1}, {Filter: contract.AlarmEventTopic(a.cfg.Site), QoS: 1}}
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
	defer context.AfterFunc(ctx, a.stop)()
	// Everything runs on life, which ends with ctx: a loop only stops once life is
	// over, so a message handed to it meanwhile is known to be given up.
	var wg sync.WaitGroup
	for _, run := range []func(context.Context){func(c context.Context) { a.loop.Run(c) }, a.out.Run} {
		wg.Go(func() { run(a.life) })
	}
	// The status at start; then on change only. It is retained: nobody needs it again
	// unchanged, and the agent sends it to the cloud, on mobile data paid by the client
	// maybe.
	a.loop.Do(a.life, func(now time.Time) []video.Action {
		a.publishStatus(now)
		return nil
	})
	wg.Wait()
	a.work.Wait() // every stream stopped
	a.out.Flush()
	return nil
}

// dispatch runs on the loop's goroutine: it starts and stops streams in their own
// goroutines, and queues the messages, without ever waiting for a camera.
func (a *App) dispatch(_ context.Context, now time.Time, actions []video.Action) error {
	for _, act := range actions {
		switch v := act.(type) {
		case video.Start:
			a.startStream(v)
		case video.Halt:
			if stop, ok := a.sessions[v.Session]; ok {
				stop()
				delete(a.sessions, v.Session)
			}
		case video.Report:
			a.log.Info("video session", "session", v.Session, "camera", v.Camera, "status", v.Status, "reason", v.Reason, "bytes", v.Bytes)
			a.out.Send(src, contract.VideoSessionTopic(a.cfg.Site), contract.TypeVideoSession, videowire.Session(v), false, now)
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

// publishStatus publishes video.status, retained, when it changed. It runs on the
// loop's goroutine.
func (a *App) publishStatus(now time.Time) {
	st := videowire.Status(a.core.State())
	if a.sent && reflect.DeepEqual(st, a.last) {
		return
	}
	a.last, a.sent = st, true
	a.out.Send(src, contract.VideoStatusTopic(a.cfg.Site), contract.TypeVideoStatus, st, true, now)
}
