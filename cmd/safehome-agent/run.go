package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"safehome/internal/adapter/filestore"
	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/photodir"
	"safehome/internal/adapter/pidfile"
	"safehome/internal/adapter/sdnotify"
	"safehome/internal/adapter/upload"
	"safehome/internal/agentapp"
	"safehome/internal/agentconfig"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/outbox"
)

const pidName = "agent.pid"

const usage = `usage:
  safehome-agent run   -config FILE   run the link agent
  safehome-agent check -config FILE   check the configuration and the cloud's secret, and exit
  safehome-agent version
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Fprintln(stdout, "safehome-agent", version)
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
	cfg, err := agentconfig.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}
	if cmd == "check" {
		fmt.Fprintf(stdout, "configuration OK: site %s, cloud %s as %s, hub broker %s, state in %s\n",
			cfg.App.Site, strings.Join(cfg.Cloud.URLs, " then "), cfg.Cloud.Username, cfg.BrokerURL, cfg.StateDir)
		return 0
	}
	return runAgent(ctx, cfg, stderr)
}

// brokerPublisher gives the agent a publisher before the MQTT client exists.
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

func subscriptions(in []agentapp.Subscription) []mqtt.Subscription {
	out := make([]mqtt.Subscription, len(in))
	for i, s := range in {
		out[i] = mqtt.Subscription{Filter: s.Filter, QoS: s.QoS, NoLocal: s.NoLocal}
	}
	return out
}

// photos fits the photos' folder to the application's port.
type photos struct{ d *photodir.Dir }

func (p photos) Open(id string) (io.ReadCloser, int64, error) {
	f, n, err := p.d.Open(id)
	if errors.Is(err, photodir.ErrNotFound) {
		return nil, 0, fmt.Errorf("%w: %w", agentapp.ErrNoPhoto, err)
	}
	return f, n, err
}

func (p photos) Remove(id string) error { return p.d.Remove(id) }

// uploader fits HTTPS to the application's port.
type uploader struct{ c *upload.Client }

func (u uploader) Put(ctx context.Context, url, token string, body io.Reader, size int64) error {
	err := u.c.Put(ctx, url, token, body, size)
	switch {
	case errors.Is(err, upload.ErrExpired):
		return fmt.Errorf("%w: %w", agentapp.ErrUploadExpired, err)
	case errors.Is(err, upload.ErrRefused):
		return fmt.Errorf("%w: %w", agentapp.ErrUploadRefused, err)
	}
	return err
}

func runAgent(ctx context.Context, cfg agentconfig.Result, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	store, err := filestore.Open(cfg.StateDir, log)
	if err != nil {
		log.Error("cannot open the state directory", "dir", cfg.StateDir, "err", err)
		return 1
	}
	release, err := pidfile.Lock(cfg.StateDir, pidName)
	if err != nil {
		log.Error("another link agent is already using this state directory", "dir", cfg.StateDir, "err", err)
		return 1
	}
	defer release()

	dir, err := photodir.Open(cfg.PhotoDir, 60, 8<<20)
	if err != nil {
		log.Error("cannot open the photos' folder", "dir", cfg.PhotoDir, "err", err)
		return 1
	}
	// The photos go to the cloud's HTTPS endpoint, under the same authority as its broker.
	https, err := upload.New(cfg.Cloud.CAFile)
	if err != nil {
		log.Error("cannot prepare the photos' upload", "err", err)
		return 1
	}
	ids := contract.NewIDGenerator()
	local, cloud := &brokerPublisher{}, &brokerPublisher{}
	app, err := agentapp.New(cfg.App, agentapp.Deps{
		Clock: driver.SystemClock(), Store: store, Local: local, Cloud: cloud,
		Photos: photos{dir}, Uploader: uploader{https}, NewID: ids.New, Logger: log,
	})
	if err != nil {
		log.Error("cannot start the agent", "err", err)
		return 1
	}
	log.Info("starting", "site", cfg.App.Site, "cloud", cfg.Cloud, "broker", cfg.BrokerURL, "version", version)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { app.Run(runCtx); close(done) }()
	fail := func(msg string, err error) int {
		log.Error(msg, "err", err)
		cancel()
		<-done
		return 1
	}

	// The clients outlive the application: at a stop, it sends what is left for the
	// hub, then the agent tells the cloud it goes offline.
	clientsCtx, closeClients := context.WithCancel(context.Background())
	defer closeClients()
	will := func() mqtt.Message {
		topic, payload := app.Will()
		return mqtt.Message{Topic: topic, Payload: payload, QoS: 1, Retain: true}
	}
	hubClient, err := mqtt.Dial(clientsCtx, mqtt.Options{
		BrokerURL: cfg.BrokerURL, ClientID: cfg.ClientID, Subscriptions: subscriptions(app.LocalSubscriptions()), Logger: log,
		OnMessage: func(m mqtt.Message) { app.HandleLocal(agentapp.Message{Topic: m.Topic, Payload: m.Payload}) },
	})
	if err != nil {
		return fail("cannot start the hub's MQTT client", err)
	}
	local.client.Store(hubClient)
	cloudClient, err := mqtt.Dial(clientsCtx, mqtt.Options{
		BrokerURL: cfg.Cloud.URLs[0], FallbackURLs: cfg.Cloud.URLs[1:], CAFile: cfg.Cloud.CAFile,
		ClientID: cfg.Cloud.ClientID, Username: cfg.Cloud.Username, Password: cfg.Cloud.Password,
		Subscriptions: subscriptions(app.CloudSubscriptions()), Will: will, Spread: true, Logger: log.With("broker", "cloud"),
		OnMessage:    func(m mqtt.Message) { app.HandleCloud(agentapp.Message{Topic: m.Topic, Payload: m.Payload}) },
		OnLinkChange: app.LinkChanged,
	})
	if err != nil {
		return fail("cannot start the cloud's MQTT client", err)
	}
	cloud.client.Store(cloudClient)

	// systemd: started, then "still working" for as long as the loop answers. A stuck
	// agent stops saying so and systemd restarts it (WatchdogSec).
	notify := sdnotify.FromEnv()
	if err := notify.Ready(); err != nil {
		log.Warn("could not tell systemd the agent is ready", "err", err)
	}
	go notify.Keepalive(runCtx, app.Alive, log)

	select {
	case <-ctx.Done():
		log.Info("stopping")
		notify.Stopping()
		cancel()
		<-done
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		// A clean disconnection does not set off the Last Will: the agent says itself
		// that the site goes offline, or the cloud would believe it still connected.
		if err := cloudClient.Publish(closeCtx, will()); err != nil {
			log.Warn("could not tell the cloud the agent stops", "err", err)
		}
		cloudClient.Close(closeCtx)
		hubClient.Close(closeCtx)
		return 0
	case <-done:
		log.Error("the agent stopped unexpectedly")
		return 1
	}
}
