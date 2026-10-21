package main

import (
	"context"
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
	"safehome/internal/consoleapp"
	"safehome/internal/consoleapp/web"
	"safehome/internal/consoleconfig"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/hubconfig"
	"safehome/internal/service"
)

const (
	name    = "console" // in the logs
	pidName = "console.pid"
)

var commandLine = service.CLI{Program: "safehome-console", Version: version, Usage: `usage:
  safehome-console run   -config FILE   run the local console
  safehome-console check -config FILE   check the configuration and exit
  safehome-console version
`}

// nmcli is NetworkManager's client; the tests use a fake.
var nmcli = "nmcli"

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd, exit, ok := commandLine.Parse(args, stdout, stderr)
	if !ok {
		return exit
	}
	cfg, hub, ok := loadConfig(cmd.Config, stderr)
	if !ok {
		return 1
	}
	if cmd.Name == "check" {
		fmt.Fprintf(stdout, "configuration OK: site %s, network %q on %s, page at http://%s/ while the installation mode lasts\n",
			cfg.App.Site, cfg.Wifi.SSID, cfg.Wifi.Interface, cfg.Host())
		if _, err := exec.LookPath(nmcli); err != nil {
			fmt.Fprintln(stdout, "warning: nmcli (NetworkManager) is not installed: the network cannot start")
		}
		return 0
	}
	return runConsole(ctx, cfg, hub, stderr)
}

// loadConfig reads the console's configuration and the hub's, and says on stderr
// what is wrong with them.
func loadConfig(path string, stderr io.Writer) (consoleconfig.Result, hubconfig.Result, bool) {
	cfg, err := consoleconfig.Load(path)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return cfg, hubconfig.Result{}, false
	}
	hub, err := hubconfig.Load(cfg.HubConfig)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error: the hub's:", err)
		return cfg, hub, false
	}
	// The zones and sirens go to the hub's state folder: the console must be allowed
	// to write there, which its service grants for its own state folder.
	if filepath.Clean(hub.StateDir) != filepath.Clean(cfg.StateDir) || hub.Hub.Site != cfg.App.Site {
		fmt.Fprintf(stderr, "configuration error: site %s and state_dir %s must be the hub's (%s, %s)\n",
			cfg.App.Site, cfg.StateDir, hub.Hub.Site, hub.StateDir)
		return cfg, hub, false
	}
	return cfg, hub, true
}

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
	log := service.Logger(stderr, cfg.LogLevel)
	release, err := service.Lock(name, cfg.StateDir, pidName)
	if err != nil {
		log.Error("cannot start", "err", err)
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
	pub := &service.Publisher{}
	var page atomic.Pointer[web.Handler] // the page needs the application, which serves it
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

	return service.Daemon{
		Name: name, Log: log, Alive: app.Alive,
		// The network is stopped on the way out of the loop.
		Loops: []func(context.Context) error{func(ctx context.Context) error { app.Run(ctx); return nil }},
		Brokers: []service.Broker{{Publisher: pub, Options: mqtt.Options{
			BrokerURL: cfg.BrokerURL, ClientID: cfg.ClientID, Subscriptions: service.Subscriptions(app.Subscriptions()), Logger: log,
			OnMessage: func(m mqtt.Message) {
				app.HandleMessage(consoleapp.Message{Topic: m.Topic, Payload: m.Payload, Retain: m.Retain})
			},
		}}},
	}.Run(ctx)
}
