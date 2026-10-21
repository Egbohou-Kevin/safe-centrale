package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/sysclock"
	"safehome/internal/configapp"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/hub"
	"safehome/internal/hubconfig"
	"safehome/internal/inbox"
	"safehome/internal/service"
	"safehome/internal/siteconfig"
)

const (
	name    = "hub" // in the logs
	pidName = "hub.pid"
)

const usage = `usage:
  safehome-hub run    -config FILE            run the hub
  safehome-hub check  -config FILE            check the configuration and exit
  safehome-hub codes  -config FILE list       list who has an access code
  safehome-hub codes  -config FILE set USER   set the code of USER (the code is read from the standard input)
  safehome-hub codes  -config FILE remove USER
  safehome-hub version

The user "installer" holds the installer code: typed then "disarm" at the keypad, it
opens the installation mode (the local console) while the alarm is disarmed. It is
apart from the people's codes and never disarms.
`

var commandLine = service.CLI{Program: "safehome-hub", Version: version, Usage: usage, Extra: []string{"codes"}}

// run is main without the process: it takes everything it needs, so tests can run
// it. It returns the exit code.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd, exit, ok := commandLine.Parse(args, stdout, stderr)
	if !ok {
		return exit
	}
	cfg, err := hubconfig.Load(cmd.Config)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}
	switch cmd.Name {
	case "check":
		check(cfg, stdout)
		return 0
	case "codes":
		return codes(cfg, cmd.Args, stdin, stdout, stderr)
	}
	return runHub(ctx, cfg, cmd.Config, stderr)
}

func check(cfg hubconfig.Result, stdout io.Writer) {
	fmt.Fprintf(stdout, "configuration OK: site %s, %d zone(s), %d siren(s), %d contact(s), state in %s\n",
		cfg.Hub.Site, len(cfg.Hub.Engine.Zones), len(cfg.Hub.SirenIDs), len(cfg.Hub.Escalation.Contacts), cfg.StateDir)
	if cfg.DevicesErr != nil {
		fmt.Fprintf(stdout, "warning: the zones and sirens set by the installer's tools are left aside: %v\n", cfg.DevicesErr)
	} else if cfg.DevicesSum != "" {
		fmt.Fprintf(stdout, "zones and sirens: set by the installer's tools (%s in %s)\n", hubconfig.DevicesFile, cfg.StateDir)
	}
	switch {
	case cfg.SiteErr != nil:
		fmt.Fprintf(stdout, "warning: the configuration from the cloud (version %d) is left aside: %v\n", cfg.SiteVersion, cfg.SiteErr)
	case cfg.SiteVersion > 0:
		fmt.Fprintf(stdout, "configuration from the cloud: version %d\n", cfg.SiteVersion)
	}
	if len(cfg.Hub.Escalation.Contacts) == 0 {
		fmt.Fprintln(stdout, "warning: no contact is configured, nobody would be told of an alarm")
	}
}

func runHub(ctx context.Context, cfg hubconfig.Result, configPath string, stderr io.Writer) int {
	log := service.Logger(stderr, cfg.LogLevel)

	// SIGHUP means "the installer's tool changed the codes, read them again". It is
	// caught before the pid file appears: a signal sent in between would otherwise
	// kill the hub, whose default reaction to SIGHUP is to exit.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	store, release, err := service.OpenState(name, cfg.StateDir, pidName, log)
	if err != nil {
		log.Error("cannot start", "err", err)
		return 1
	}
	defer release()
	if cfg.DevicesErr != nil {
		log.Error("the zones and sirens set by the installer's tools are left aside: the hub runs on its own file", "err", cfg.DevicesErr)
	}

	ids := contract.NewIDGenerator()
	pub := &service.Publisher{}
	restart := service.NewRestart()
	h, err := newHub(cfg, configPath, hub.Deps{Store: store, Publisher: pub, NewID: ids.New, Logger: log})
	if err != nil {
		log.Error("cannot start the hub", "err", err)
		return 1
	}
	log.Info("starting", "site", cfg.Hub.Site, "broker", cfg.BrokerURL, "version", version)
	conf, err := newConfigApp(cfg, configPath, configapp.Deps{Store: store, Publisher: pub, NewID: ids.New, Logger: log,
		Restart: restart.Ask}, func() {
		// The contacts may have changed and the hub may not restart yet (the
		// installation mode): their codes follow now.
		if err := h.ReloadCodes(); err != nil {
			log.Error("reloading the access codes failed, the previous ones stay in use", "err", err)
		}
	})
	if err != nil {
		log.Error("cannot start the configuration from the cloud", "err", err)
		return 1
	}
	return daemon(cfg, h, conf, pub, restart, hup, log).Run(ctx)
}

// newHub builds the hub on what the program gives it (deps), with the real clock. The
// codes it takes are those of the contacts saved now, read like the console does: the
// configuration from the cloud may have changed them since the start.
func newHub(cfg hubconfig.Result, configPath string, deps hub.Deps) (*hub.Hub, error) {
	deps.Clock = driver.SystemClock()
	deps.ClockSynced = sysclock.Synced
	deps.Contacts = func() ([]string, error) {
		current, err := hubconfig.Load(configPath)
		if err != nil {
			return nil, err
		}
		var ids []string
		for _, c := range current.Hub.Escalation.Contacts {
			ids = append(ids, c.ID)
		}
		return ids, nil
	}
	return hub.New(cfg.Hub, deps)
}

// devicesChanged says whether the installer changed the zones or the sirens (the
// local console, the command-line tool) since the start: the hub restarts to load
// them, as for a configuration from the cloud. A file the hub left aside counts as
// the one it started with: it must not restart for it again and again.
func devicesChanged(cfg hubconfig.Result) func() bool {
	loaded := cfg.DevicesSum
	if cfg.DevicesErr != nil {
		loaded = hubconfig.DevicesSum(cfg.StateDir)
	}
	return func() bool { return hubconfig.DevicesSum(cfg.StateDir) != loaded }
}

// newConfigApp builds the application of the configuration set from the cloud:
// checked against the hub's file, kept in the state folder, applied by restarting
// (systemd starts the hub again). It restarts the hub for new zones or sirens too.
// saved runs after each configuration kept.
func newConfigApp(cfg hubconfig.Result, configPath string, deps configapp.Deps, saved func()) (*configapp.App, error) {
	ccfg := configapp.DefaultConfig(cfg.Hub.Site, cfg.SiteVersion)
	if cfg.SiteErr != nil {
		ccfg.Running = 0
		if cfg.SiteVersion > 0 {
			ccfg.Refused, ccfg.RefusedBecause = cfg.SiteVersion, cfg.SiteErr.Error()
		} else {
			deps.Logger.Error("the configuration from the cloud cannot be read: the hub runs on its own file", "err", cfg.SiteErr)
		}
	}
	deps.Clock = driver.SystemClock()
	deps.Check = func(d contract.ConfigSetData) error {
		_, err := hubconfig.WithSiteConfig(configPath, d)
		return err
	}
	deps.Save = func(d contract.ConfigSetData) error {
		if err := siteconfig.Write(cfg.StateDir, d); err != nil {
			return err
		}
		saved()
		return nil
	}
	deps.DevicesChanged = devicesChanged(cfg)
	return configapp.New(ccfg, deps)
}

// daemon runs the hub and the configuration's application on one broker client:
// each message goes to the one that subscribed to its topic.
func daemon(cfg hubconfig.Result, h *hub.Hub, conf *configapp.App, pub *service.Publisher, restart service.Restart,
	hup <-chan os.Signal, log *slog.Logger) service.Daemon {
	confSubs := service.Subscriptions(conf.Subscriptions())
	confTopics := map[string]bool{}
	for _, s := range confSubs {
		confTopics[s.Filter] = true
	}
	subs := append(service.Subscriptions(h.Subscriptions()), confSubs...)
	return service.Daemon{
		Name: name, Log: log, Alive: h.Alive, Restart: restart,
		Loops: []func(context.Context) error{
			h.Run,
			conf.Run,
			func(ctx context.Context) error { reloadCodes(ctx, h, hup, log); return nil },
		},
		Brokers: []service.Broker{{Publisher: pub, Options: mqtt.Options{
			BrokerURL: cfg.BrokerURL, ClientID: cfg.ClientID, Subscriptions: subs, Logger: log,
			OnSubscribed: h.LinkUp,
			OnMessage: service.Deliver(func(m inbox.Message) {
				if confTopics[m.Topic] { // exact topics, no wildcard
					conf.HandleMessage(m)
					return
				}
				h.HandleMessage(m)
			}),
		}}},
	}
}

// reloadCodes reads the access codes again at each SIGHUP, until ctx ends.
func reloadCodes(ctx context.Context, h *hub.Hub, hup <-chan os.Signal, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			if err := h.ReloadCodes(); err != nil {
				log.Error("reloading the access codes failed, the previous ones stay in use", "err", err)
			}
		}
	}
}
