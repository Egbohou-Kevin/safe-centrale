package main

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/gateway"
	"safehome/internal/gsmconfig"
	"safehome/internal/service"
	"safehome/internal/siteconfig"
)

const (
	name    = "gateway" // in the logs
	pidName = "gsm.pid"
)

var commandLine = service.CLI{Program: "safehome-gsm", Version: version, Usage: `usage:
  safehome-gsm run   -config FILE   run the GSM gateway
  safehome-gsm check -config FILE   check the configuration and exit
  safehome-gsm version
`}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd, exit, ok := commandLine.Parse(args, stdout, stderr)
	if !ok {
		return exit
	}
	cfg, err := gsmconfig.Load(cmd.Config)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}
	if cmd.Name == "check" {
		check(cfg, stdout)
		return 0
	}
	return runGateway(ctx, cfg, stderr)
}

func check(cfg gsmconfig.Result, stdout io.Writer) {
	g := cfg.Gateway.GSM
	fmt.Fprintf(stdout, "configuration OK: site %s, %d line(s), %d contact(s), state in %s\n",
		cfg.Gateway.Site, len(g.Lines), len(g.Contacts), cfg.StateDir)
	switch {
	case cfg.SiteErr != nil:
		fmt.Fprintf(stdout, "warning: the configuration from the cloud (version %d) is left aside: %v\n", cfg.SiteVersion, cfg.SiteErr)
	case cfg.SiteVersion > 0:
		fmt.Fprintf(stdout, "configuration from the cloud: version %d\n", cfg.SiteVersion)
	}
	if g.TestNumber == "" {
		fmt.Fprintln(stdout, "warning: no test number, a suspended SIM will only be noticed when an alert fails")
	}
}

func runGateway(ctx context.Context, cfg gsmconfig.Result, stderr io.Writer) int {
	log := service.Logger(stderr, cfg.LogLevel)
	store, release, err := service.OpenState(name, cfg.StateDir, pidName, log)
	if err != nil {
		log.Error("cannot start", "err", err)
		return 1
	}
	defer release()

	var gw atomic.Pointer[gateway.Gateway]
	lines, closeModems := openModems(cfg, &gw, log)
	defer closeModems()
	ids := contract.NewIDGenerator()
	pub := &service.Publisher{}
	cfg.Gateway.Contacts = siteContacts(cfg)
	g, err := gateway.New(cfg.Gateway, gateway.Deps{
		Clock: driver.SystemClock(), Store: store, Publisher: pub, NewID: ids.New, Logger: log, Lines: lines,
	})
	if err != nil {
		log.Error("cannot start the gateway", "err", err)
		return 1
	}
	gw.Store(g)
	log.Info("starting", "site", cfg.Gateway.Site, "lines", len(lines), "broker", cfg.BrokerURL, "version", version, "site_config", cfg.SiteVersion)
	if cfg.SiteErr != nil {
		log.Error("the configuration from the cloud is left aside: the gateway runs on its own file", "site_config", cfg.SiteVersion, "err", cfg.SiteErr)
	}

	// The contacts' numbers may come from the cloud: once the hub runs with a new
	// configuration, the gateway restarts to load it (systemd starts it again).
	applied := contract.ConfigAppliedTopic(cfg.Gateway.Site)
	restart := service.NewRestart()
	return service.Daemon{
		Name: name, Log: log, Loops: []func(context.Context) error{g.Run}, Alive: g.Alive, Restart: restart,
		Brokers: []service.Broker{{Publisher: pub, Options: mqtt.Options{
			BrokerURL: cfg.BrokerURL, ClientID: cfg.ClientID, Logger: log,
			Subscriptions: append(service.Subscriptions(g.Subscriptions()), mqtt.Subscription{Filter: applied, QoS: 1}),
			OnMessage: func(m mqtt.Message) {
				if m.Topic != applied {
					g.HandleMessage(gateway.Message{Topic: m.Topic, Payload: m.Payload})
				} else if v, ok := siteconfig.Changed(cfg.StateDir, cfg.Gateway.Site, cfg.SiteVersion, m.Payload); ok {
					log.Info("restarting to load the configuration from the cloud", "site_config", v)
					restart.Ask()
				}
			},
		}}},
	}.Run(ctx)
}

// siteContacts lists every contact of the configuration from the cloud the gateway
// runs with, those reached by the app only too: an acknowledgement from any of them
// ends the incident for the escalation, so it does for the gateway. Nil (no such
// configuration, or one without contacts): the gateway knows the contacts with a
// number only.
func siteContacts(cfg gsmconfig.Result) []string {
	if cfg.SiteVersion == 0 || cfg.SiteErr != nil {
		return nil
	}
	o, ok, err := siteconfig.Read(cfg.StateDir)
	if err != nil || !ok || o.Version != cfg.SiteVersion || o.Contacts == nil {
		return nil
	}
	ids := make([]string, 0, len(o.Contacts))
	for _, c := range o.Contacts {
		ids = append(ids, c.ID)
	}
	return ids
}
