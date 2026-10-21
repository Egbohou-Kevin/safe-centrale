package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/sshtunnel"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/maintapp"
	"safehome/internal/maintconfig"
	"safehome/internal/service"
)

const (
	name    = "maintenance service" // in the logs
	pidName = "maint.pid"
)

var commandLine = service.CLI{Program: "safehome-maint", Version: version, Usage: `usage:
  safehome-maint run   -config FILE   run the remote maintenance service
  safehome-maint check -config FILE   check the configuration and the keys, and exit
  safehome-maint version
`}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd, exit, ok := commandLine.Parse(args, stdout, stderr)
	if !ok {
		return exit
	}
	cfg, err := maintconfig.Load(cmd.Config)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}
	if cmd.Name == "check" {
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

// newTunneler prepares ssh, unless remote maintenance is switched off: the service
// then refuses every session.
func newTunneler(cfg maintconfig.Result, log *slog.Logger) (tunneler, error) {
	if !cfg.App.Maint.Enabled {
		return tunneler{}, nil
	}
	t := cfg.Tunnel
	r, err := sshtunnel.New(sshtunnel.Options{
		Binary: t.SSH, Host: t.Host, Port: t.Port, User: t.User, KeyFile: t.KeyFile, KnownHosts: t.KnownHosts,
		RemotePort: t.RemotePort, LocalPort: t.LocalPort, Logger: log,
	})
	return tunneler{r}, err
}

func runService(ctx context.Context, cfg maintconfig.Result, stderr io.Writer) int {
	log := service.Logger(stderr, cfg.LogLevel)
	store, release, err := service.OpenState(name, cfg.StateDir, pidName, log)
	if err != nil {
		log.Error("cannot start", "err", err)
		return 1
	}
	defer release()

	tun, err := newTunneler(cfg, log)
	if err != nil {
		log.Error("cannot prepare the tunnel", "err", err)
		return 1
	}
	ids := contract.NewIDGenerator()
	pub := &service.Publisher{}
	app, err := maintapp.New(cfg.App, maintapp.Deps{
		Clock: driver.SystemClock(), Store: store, Publisher: pub, Tunneler: tun, NewID: ids.New, Logger: log,
	})
	if err != nil {
		log.Error("cannot start the maintenance service", "err", err)
		return 1
	}
	log.Info("starting", "site", cfg.App.Site, "enabled", cfg.App.Maint.Enabled, "bastion", cfg.Tunnel.Host, "broker", cfg.BrokerURL, "version", version)

	return service.Daemon{
		Name: name, Log: log, Loops: []func(context.Context) error{app.Run}, Alive: app.Alive,
		Brokers: []service.Broker{{Publisher: pub, Options: mqtt.Options{
			BrokerURL: cfg.BrokerURL, ClientID: cfg.ClientID, Subscriptions: service.Subscriptions(app.Subscriptions()), Logger: log,
			OnMessage: func(m mqtt.Message) { app.HandleMessage(maintapp.Message{Topic: m.Topic, Payload: m.Payload}) },
		}}},
	}.Run(ctx)
}
