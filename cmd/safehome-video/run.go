package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"safehome/internal/adapter/filestore"
	"safehome/internal/adapter/go2rtc"
	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/photodir"
	"safehome/internal/adapter/pidfile"
	"safehome/internal/adapter/sdnotify"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/outbox"
	"safehome/internal/siteconfig"
	"safehome/internal/videoapp"
	"safehome/internal/videoconfig"
)

const pidName = "video.pid"

const usage = `usage:
  safehome-video run   -config FILE   run the video service
  safehome-video check -config FILE   check the configuration and go2rtc, and exit
  safehome-video version
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Fprintln(stdout, "safehome-video", version)
		return 0
	case "run", "check":
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s", cmd, usage)
		return 2
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "configuration file (required)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if *configPath == "" {
		fmt.Fprintf(stderr, "%s: -config is required\n", cmd)
		return 2
	}
	cfg, err := videoconfig.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}
	if fi, err := os.Stat(cfg.Go2rtc); err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
		fmt.Fprintf(stderr, "go2rtc is not installed at %s (see docs/systeme.md)\n", cfg.Go2rtc)
		return 1
	}
	if cmd == "check" {
		v := cfg.App.Video
		uplink := "fixed line, no allowance"
		if v.Mobile {
			uplink = fmt.Sprintf("mobile data, %d MB a month, sessions of %v at most", v.Quota>>20, v.MaxLength)
		}
		fmt.Fprintf(stdout, "configuration OK: site %s, %d camera(s), %s, go2rtc %s, state in %s\n",
			cfg.App.Site, len(v.Cameras), uplink, cfg.Go2rtc, cfg.StateDir)
		if p := v.Photos; p.Count > 0 {
			fmt.Fprintf(stdout, "photos at an alarm: %d per camera, %d px wide\n", p.Count, p.Width)
			if fi, err := os.Stat(cfg.FFmpeg); err != nil || fi.IsDir() {
				fmt.Fprintf(stdout, "warning: ffmpeg is not installed at %s: no photo at an alarm (see docs/systeme.md)\n", cfg.FFmpeg)
			}
		}
		switch {
		case cfg.SiteErr != nil:
			fmt.Fprintf(stdout, "warning: the configuration from the cloud (version %d) is left aside: %v\n", cfg.SiteVersion, cfg.SiteErr)
		case cfg.SiteVersion > 0:
			fmt.Fprintf(stdout, "configuration from the cloud: version %d\n", cfg.SiteVersion)
		}
		if cfg.SiteNote != "" {
			fmt.Fprintln(stdout, "note:", cfg.SiteNote)
		}
		return 0
	}
	return runService(ctx, cfg, stderr)
}

// brokerPublisher gives the service a publisher before the MQTT client exists.
type brokerPublisher struct{ client atomic.Pointer[mqtt.Client] }

func (b *brokerPublisher) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	c := b.client.Load()
	if c == nil {
		return outbox.ErrUnavailable
	}
	err := c.Publish(ctx, mqtt.Message{Topic: topic, Payload: payload, QoS: qos, Retain: retain})
	if errors.Is(err, mqtt.ErrDown) {
		return outbox.ErrUnavailable
	}
	return err
}

// streamer fits go2rtc to the application's Streamer port.
type streamer struct{ r *go2rtc.Runner }

func (s streamer) Start(ctx context.Context, camera, target string) (videoapp.Stream, error) {
	st, err := s.r.Start(ctx, camera, target)
	if errors.Is(err, go2rtc.ErrCameraUnreachable) {
		return nil, fmt.Errorf("%w: %w", videoapp.ErrCameraUnreachable, err)
	}
	if err != nil {
		return nil, err
	}
	return st, nil
}

func (s streamer) Probe(ctx context.Context, camera string) error { return s.r.Probe(ctx, camera) }

// photographer fits go2rtc to the application's Photographer port.
type photographer struct{ r *go2rtc.Runner }

func (p photographer) Open(ctx context.Context) (videoapp.Shooter, error) {
	s, err := p.r.OpenStills(ctx)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func runService(ctx context.Context, cfg videoconfig.Result, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	store, err := filestore.Open(cfg.StateDir, log)
	if err != nil {
		log.Error("cannot open the state directory", "dir", cfg.StateDir, "err", err)
		return 1
	}
	release, err := pidfile.Lock(cfg.StateDir, pidName)
	if err != nil {
		log.Error("another video service is already using this state directory", "dir", cfg.StateDir, "err", err)
		return 1
	}
	defer release()

	runner, err := go2rtc.New(go2rtc.Options{Binary: cfg.Go2rtc, FFmpeg: cfg.FFmpeg, Sources: cfg.Sources, Logger: log})
	if err != nil {
		log.Error("cannot use go2rtc", "err", err)
		return 1
	}
	// The photos wait there for the link agent: 60 at most, 8 MB in all.
	photos, err := photodir.Open(cfg.PhotoDir, 60, 8<<20)
	if err != nil {
		log.Error("cannot open the photos' folder", "dir", cfg.PhotoDir, "err", err)
		return 1
	}
	ids := contract.NewIDGenerator()
	pub := &brokerPublisher{}
	app, err := videoapp.New(cfg.App, videoapp.Deps{
		Clock: driver.SystemClock(), Store: store, Publisher: pub, Streamer: streamer{runner},
		Camera: photographer{runner}, Photos: photos, NewID: ids.New, Logger: log,
	})
	if err != nil {
		log.Error("cannot start the video service", "err", err)
		return 1
	}
	log.Info("starting", "site", cfg.App.Site, "cameras", len(cfg.Sources), "mobile", cfg.App.Video.Mobile, "broker", cfg.BrokerURL,
		"version", version, "site_config", cfg.SiteVersion)
	if cfg.SiteErr != nil {
		log.Error("the configuration from the cloud is left aside: the service runs on its own file", "site_config", cfg.SiteVersion, "err", cfg.SiteErr)
	}
	if cfg.SiteNote != "" {
		log.Info(cfg.SiteNote, "site_config", cfg.SiteVersion)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { app.Run(runCtx); close(done) }()

	// The client outlives the application, which sends its last messages as it stops.
	clientCtx, closeClient := context.WithCancel(context.Background())
	defer closeClient()
	// The allowance and the longest session may come from the cloud: once the hub runs
	// with a new configuration, the service restarts to load it (systemd starts it
	// again). A session under way ends; the viewer asks again.
	applied := contract.ConfigAppliedTopic(cfg.App.Site)
	restart := make(chan struct{}, 1)
	subs := []mqtt.Subscription{{Filter: applied, QoS: 1}}
	for _, s := range app.Subscriptions() {
		subs = append(subs, mqtt.Subscription{Filter: s.Filter, QoS: s.QoS})
	}
	client, err := mqtt.Dial(clientCtx, mqtt.Options{
		BrokerURL: cfg.BrokerURL, ClientID: cfg.ClientID, Subscriptions: subs, Logger: log,
		OnMessage: func(m mqtt.Message) {
			if m.Topic == applied {
				if v, ok := siteconfig.Changed(cfg.StateDir, cfg.App.Site, cfg.SiteVersion, m.Payload); ok {
					log.Info("restarting to load the configuration from the cloud", "site_config", v)
					select {
					case restart <- struct{}{}:
					default:
					}
				}
				return
			}
			app.HandleMessage(videoapp.Message{Topic: m.Topic, Payload: m.Payload})
		},
	})
	if err != nil {
		log.Error("cannot start the MQTT client", "err", err)
		cancel()
		<-done
		return 1
	}
	pub.client.Store(client)

	// systemd: started, then "still working" for as long as the loop answers. A stuck
	// service stops saying so and systemd restarts it (WatchdogSec).
	notify := sdnotify.FromEnv()
	if err := notify.Ready(); err != nil {
		log.Warn("could not tell systemd the service is ready", "err", err)
	}
	go notify.Keepalive(runCtx, app.Alive, log)

	select {
	case <-ctx.Done():
		log.Info("stopping")
	case <-restart:
	case <-done:
		log.Error("the video service stopped unexpectedly")
		return 1
	}
	notify.Stopping()
	cancel()
	<-done
	closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	client.Close(closeCtx)
	stop()
	return 0
}
