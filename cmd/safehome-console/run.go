package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"safehome/internal/adapter/hotspot"
	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/pidfile"
	"safehome/internal/adapter/sdnotify"
	"safehome/internal/consoleapp"
	"safehome/internal/consoleapp/web"
	"safehome/internal/consoleconfig"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/hubconfig"
	"safehome/internal/install"
	"safehome/internal/outbox"
)

const pidName = "console.pid"

const usage = `usage:
  safehome-console run   -config FILE   run the local console
  safehome-console check -config FILE   check the configuration and exit
  safehome-console version
`

// nmcli is NetworkManager's client; the tests use a fake.
var nmcli = "nmcli"

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Fprintln(stdout, "safehome-console", version)
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
	cfg, err := consoleconfig.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}
	hub, err := hubconfig.Load(cfg.HubConfig)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error: the hub's:", err)
		return 1
	}
	// The zones and sirens go to the hub's state folder: the console must be allowed
	// to write there, which its service grants for its own state folder.
	if filepath.Clean(hub.StateDir) != filepath.Clean(cfg.StateDir) || hub.Hub.Site != cfg.App.Site {
		fmt.Fprintf(stderr, "configuration error: site %s and state_dir %s must be the hub's (%s, %s)\n",
			cfg.App.Site, cfg.StateDir, hub.Hub.Site, hub.StateDir)
		return 1
	}
	if cmd == "check" {
		fmt.Fprintf(stdout, "configuration OK: site %s, network %q on %s, page at http://%s/ while the installation mode lasts\n",
			cfg.App.Site, cfg.Wifi.SSID, cfg.Wifi.Interface, cfg.Host())
		if _, err := exec.LookPath(nmcli); err != nil {
			fmt.Fprintln(stdout, "warning: nmcli (NetworkManager) is not installed: the network cannot start")
		}
		return 0
	}
	return runConsole(ctx, cfg, hub, stderr)
}

// brokerPublisher gives the console a publisher before the MQTT client exists.
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

// sites fits the hub's files (hubconfig) to the console's Sites port.
type sites struct{ hubConfig string }

func (s sites) Read() (install.Site, error) {
	h, err := hubconfig.ReadSite(s.hubConfig)
	if err != nil {
		return install.Site{}, err
	}
	out := install.Site{Zones: map[string]install.Zone{}, SirenIDs: h.SirenIDs}
	for id, z := range h.Zones {
		out.Zones[id] = install.Zone{Name: z.Name, Kind: z.Kind}
	}
	return out, nil
}

func toHub(s install.Site) hubconfig.Site {
	out := hubconfig.Site{Zones: map[string]hubconfig.Zone{}, SirenIDs: s.SirenIDs}
	for id, z := range s.Zones {
		out.Zones[id] = hubconfig.Zone{Name: z.Name, Kind: z.Kind}
	}
	return out
}

func (s sites) Check(site install.Site) error { return hubconfig.CheckSite(s.hubConfig, toHub(site)) }
func (s sites) Write(site install.Site) error { return hubconfig.WriteSite(s.hubConfig, toHub(site)) }

// serve serves the page on addr until ctx ends. The address exists only once the
// network is up: it is tried for a few seconds.
func serve(ctx context.Context, addr string, h http.Handler, log *slog.Logger) error {
	var ln net.Listener
	var err error
	for range 20 {
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 8 << 10,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	log.Info("the console's page is served", "address", "http://"+addr+"/")
	select {
	case <-ctx.Done():
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		srv.Shutdown(stopCtx)
		return nil
	case err := <-done:
		return err
	}
}

func runConsole(ctx context.Context, cfg consoleconfig.Result, hub hubconfig.Result, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	release, err := pidfile.Lock(cfg.StateDir, pidName)
	if err != nil {
		log.Error("another console is already using this state directory", "dir", cfg.StateDir, "err", err)
		return 1
	}
	defer release()

	spot, err := hotspot.New(hotspot.Config{Interface: cfg.Wifi.Interface, SSID: cfg.Wifi.SSID, Passphrase: cfg.Wifi.Passphrase,
		Address: cfg.Wifi.Address, NMCLI: nmcli})
	if err != nil {
		log.Error("cannot prepare the network", "err", err)
		return 1
	}
	ids := contract.NewIDGenerator()
	pub := &brokerPublisher{}
	var page atomic.Pointer[web.Handler]
	app, err := consoleapp.New(cfg.App, consoleapp.Deps{
		Clock: driver.SystemClock(), Publisher: pub, NewID: ids.New, Hotspot: spot, Sites: sites{hubConfig: cfg.HubConfig},
		Codes: codes{hubConfig: cfg.HubConfig, log: log}, Logger: log,
		Serve: func(ctx context.Context) error { return serve(ctx, cfg.Listen(), page.Load(), log) },
	})
	if err != nil {
		log.Error("cannot start the console", "err", err)
		return 1
	}
	h, err := web.New(app, cfg.Host(), hub.Hub.Engine.Location)
	if err != nil {
		log.Error("cannot build the page", "err", err)
		return 1
	}
	page.Store(h)
	log.Info("starting", "site", cfg.App.Site, "wifi", cfg.Wifi, "broker", cfg.BrokerURL, "version", version)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { app.Run(runCtx); close(done) }()

	var subs []mqtt.Subscription
	for _, s := range app.Subscriptions() {
		subs = append(subs, mqtt.Subscription{Filter: s.Filter, QoS: s.QoS})
	}
	client, err := mqtt.Dial(runCtx, mqtt.Options{
		BrokerURL: cfg.BrokerURL, ClientID: cfg.ClientID, Subscriptions: subs, Logger: log,
		OnMessage: func(m mqtt.Message) {
			app.HandleMessage(consoleapp.Message{Topic: m.Topic, Payload: m.Payload, Retain: m.Retain})
		},
	})
	if err != nil {
		log.Error("cannot start the MQTT client", "err", err)
		cancel()
		<-done
		return 1
	}
	pub.client.Store(client)

	// systemd: started, then "still working" for as long as the loop answers.
	notify := sdnotify.FromEnv()
	if err := notify.Ready(); err != nil {
		log.Warn("could not tell systemd the console is ready", "err", err)
	}
	go notify.Keepalive(runCtx, app.Alive, log)

	select {
	case <-ctx.Done():
		log.Info("stopping")
	case <-done:
		log.Error("the console stopped unexpectedly")
		return 1
	}
	notify.Stopping()
	cancel()
	<-done // the network is stopped on the way out
	closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	client.Close(closeCtx)
	stop()
	return 0
}
