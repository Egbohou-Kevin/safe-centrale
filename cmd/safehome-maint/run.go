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
	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/pidfile"
	"safehome/internal/adapter/sdnotify"
	"safehome/internal/adapter/sshtunnel"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/maintapp"
	"safehome/internal/maintconfig"
	"safehome/internal/outbox"
)

const pidName = "maint.pid"

const usage = `usage:
  safehome-maint run   -config FILE   run the remote maintenance service
  safehome-maint check -config FILE   check the configuration and the keys, and exit
  safehome-maint version
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Fprintln(stdout, "safehome-maint", version)
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
	cfg, err := maintconfig.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}
	if cmd == "check" {
		return check(cfg, stdout, stderr)
	}
	return runService(ctx, cfg, stderr)
}

// check says what the service would do, and whether the files it needs are there.
func check(cfg maintconfig.Result, stdout, stderr io.Writer) int {
	if !cfg.App.Maint.Enabled {
		fmt.Fprintf(stdout, "configuration OK: site %s, remote maintenance switched off\n", cfg.App.Site)
		return 0
	}
	t := cfg.Tunnel
	fmt.Fprintf(stdout, "configuration OK: site %s, tunnel to %s@%s:%d, port %d there, %v at most, state in %s\n",
		cfg.App.Site, t.User, t.Host, t.Port, t.RemotePort, cfg.App.Maint.MaxLength, cfg.StateDir)
	code := 0
	for _, f := range []struct{ what, path string }{{"the ssh client", t.SSH}, {"the hub's key", t.KeyFile}, {"the bastion's host key", t.KnownHosts}} {
		if fi, err := os.Stat(f.path); err != nil || fi.IsDir() {
			fmt.Fprintf(stderr, "%s is missing at %s (see docs/maintenance.md)\n", f.what, f.path)
			code = 1
		}
	}
	return code
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

// tunneler fits ssh to the application's Tunneler port.
type tunneler struct{ r *sshtunnel.Runner }

func (t tunneler) Open(ctx context.Context) (maintapp.Tunnel, error) {
	if t.r == nil {
		return nil, errors.New("remote maintenance is switched off")
	}
	tun, err := t.r.Open(ctx)
	if err != nil {
		return nil, err
	}
	return tun, nil
}

func runService(ctx context.Context, cfg maintconfig.Result, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	store, err := filestore.Open(cfg.StateDir, log)
	if err != nil {
		log.Error("cannot open the state directory", "dir", cfg.StateDir, "err", err)
		return 1
	}
	release, err := pidfile.Lock(cfg.StateDir, pidName)
	if err != nil {
		log.Error("another maintenance service is already using this state directory", "dir", cfg.StateDir, "err", err)
		return 1
	}
	defer release()

	var tun tunneler
	if cfg.App.Maint.Enabled {
		t := cfg.Tunnel
		if tun.r, err = sshtunnel.New(sshtunnel.Options{
			Binary: t.SSH, Host: t.Host, Port: t.Port, User: t.User, KeyFile: t.KeyFile, KnownHosts: t.KnownHosts,
			RemotePort: t.RemotePort, LocalPort: t.LocalPort, Logger: log,
		}); err != nil {
			log.Error("cannot prepare the tunnel", "err", err)
			return 1
		}
	}
	ids := contract.NewIDGenerator()
	pub := &brokerPublisher{}
	app, err := maintapp.New(cfg.App, maintapp.Deps{
		Clock: driver.SystemClock(), Store: store, Publisher: pub, Tunneler: tun, NewID: ids.New, Logger: log,
	})
	if err != nil {
		log.Error("cannot start the maintenance service", "err", err)
		return 1
	}
	log.Info("starting", "site", cfg.App.Site, "enabled", cfg.App.Maint.Enabled, "bastion", cfg.Tunnel.Host, "broker", cfg.BrokerURL, "version", version)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { app.Run(runCtx); close(done) }()

	// The client outlives the application, which sends its last messages as it stops.
	clientCtx, closeClient := context.WithCancel(context.Background())
	defer closeClient()
	var subs []mqtt.Subscription
	for _, s := range app.Subscriptions() {
		subs = append(subs, mqtt.Subscription{Filter: s.Filter, QoS: s.QoS})
	}
	client, err := mqtt.Dial(clientCtx, mqtt.Options{
		BrokerURL: cfg.BrokerURL, ClientID: cfg.ClientID, Subscriptions: subs, Logger: log,
		OnMessage: func(m mqtt.Message) { app.HandleMessage(maintapp.Message{Topic: m.Topic, Payload: m.Payload}) },
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
	case <-done:
		log.Error("the maintenance service stopped unexpectedly")
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
