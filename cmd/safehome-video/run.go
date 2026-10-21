package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"safehome/internal/adapter/go2rtc"
	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/photodir"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/service"
	"safehome/internal/siteconfig"
	"safehome/internal/videoapp"
	"safehome/internal/videoconfig"
)

const (
	name    = "video service" // in the logs
	pidName = "video.pid"
)

var commandLine = service.CLI{Program: "safehome-video", Version: version, Usage: `usage:
  safehome-video run   -config FILE   run the video service
  safehome-video check -config FILE   check the configuration and go2rtc, and exit
  safehome-video version
`}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd, exit, ok := commandLine.Parse(args, stdout, stderr)
	if !ok {
		return exit
	}
	cfg, err := videoconfig.Load(cmd.Config)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}
	if fi, err := os.Stat(cfg.Go2rtc); err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
		fmt.Fprintf(stderr, "go2rtc is not installed at %s (see docs/systeme.md)\n", cfg.Go2rtc)
		return 1
	}
	if cmd.Name == "check" {
		check(cfg, stdout)
		return 0
	}
	return runService(ctx, cfg, stderr)
}

func check(cfg videoconfig.Result, stdout io.Writer) {
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
	log := service.Logger(stderr, cfg.LogLevel)
	store, release, err := service.OpenState(name, cfg.StateDir, pidName, log)
	if err != nil {
		log.Error("cannot start", "err", err)
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
	pub := &service.Publisher{}
	app, err := videoapp.New(cfg.App, videoapp.Deps{
		Clock: driver.SystemClock(), Store: store, Publisher: pub, Streamer: streamer{runner},
		Camera: photographer{runner}, Photos: photos, NewID: ids.New, Logger: log,
	})
	if err != nil {
		log.Error("cannot start the video service", "err", err)
		return 1
	}
	logStart(cfg, log)
	return daemon(cfg, app, pub, log).Run(ctx)
}

func logStart(cfg videoconfig.Result, log *slog.Logger) {
	log.Info("starting", "site", cfg.App.Site, "cameras", len(cfg.Sources), "mobile", cfg.App.Video.Mobile, "broker", cfg.BrokerURL,
		"version", version, "site_config", cfg.SiteVersion)
	if cfg.SiteErr != nil {
		log.Error("the configuration from the cloud is left aside: the service runs on its own file", "site_config", cfg.SiteVersion, "err", cfg.SiteErr)
	}
	if cfg.SiteNote != "" {
		log.Info(cfg.SiteNote, "site_config", cfg.SiteVersion)
	}
}

// daemon runs the application on the broker. The allowance and the longest session
// may come from the cloud: once the hub runs with a new configuration, the service
// restarts to load it (systemd starts it again). A session under way ends; the
// viewer asks again.
func daemon(cfg videoconfig.Result, app *videoapp.App, pub *service.Publisher, log *slog.Logger) service.Daemon {
	applied := contract.ConfigAppliedTopic(cfg.App.Site)
	changed := func(p []byte) (int, bool) { return siteconfig.Changed(cfg.StateDir, cfg.App.Site, cfg.SiteVersion, p) }
	restart := service.NewRestart()
	return service.Daemon{
		Name: name, Log: log, Loops: []func(context.Context) error{app.Run}, Alive: app.Alive, Restart: restart,
		Brokers: []service.Broker{{Publisher: pub, Options: mqtt.Options{
			BrokerURL: cfg.BrokerURL, ClientID: cfg.ClientID, Logger: log,
			Subscriptions: append(service.Subscriptions(app.Subscriptions()), mqtt.Subscription{Filter: applied, QoS: 1}),
			OnMessage:     service.Deliver(restart.Watch(applied, changed, app.HandleMessage, log)),
		}}},
	}
}
