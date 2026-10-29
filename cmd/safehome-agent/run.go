package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/photodir"
	"safehome/internal/adapter/queuedir"
	"safehome/internal/adapter/upload"
	"safehome/internal/agentapp"
	"safehome/internal/agentconfig"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/service"
)

const (
	name      = "link agent" // in the logs
	pidName   = "agent.pid"
	queueName = "agent_queue" // the folder of the queue for the cloud, in the state directory
)

var commandLine = service.CLI{Program: "safehome-agent", Version: version, Usage: `usage:
  safehome-agent run   -config FILE   run the link agent
  safehome-agent check -config FILE   check the configuration and the cloud's secret, and exit
  safehome-agent version
`}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd, exit, ok := commandLine.Parse(args, stdout, stderr)
	if !ok {
		return exit
	}
	cfg, err := agentconfig.Load(cmd.Config)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}
	if cmd.Name == "check" {
		fmt.Fprintf(stdout, "configuration OK: site %s, cloud %s as %s, hub broker %s, state in %s\n",
			cfg.App.Site, strings.Join(cfg.Cloud.URLs, " then "), cfg.Cloud.Username, cfg.BrokerURL, cfg.StateDir)
		return 0
	}
	return runAgent(ctx, cfg, stderr)
}

func runAgent(ctx context.Context, cfg agentconfig.Result, stderr io.Writer) int {
	log := service.Logger(stderr, cfg.LogLevel)
	store, release, err := service.OpenState(name, cfg.StateDir, pidName, log)
	if err != nil {
		log.Error("cannot start", "err", err)
		return 1
	}
	defer release()
	queue, err := queuedir.Open(filepath.Join(cfg.StateDir, queueName), log)
	if err != nil {
		log.Error("cannot open the queue for the cloud", "err", err)
		return 1
	}

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
	key, err := codeKey(cfg.StateDir, log)
	if err != nil {
		log.Error("cannot read or make the key for the sealed codes", "err", err)
		return 1
	}
	ids := contract.NewIDGenerator()
	local, cloud := &service.Publisher{}, &service.Publisher{}
	app, err := agentapp.New(cfg.App, agentapp.Deps{
		Clock: driver.SystemClock(), Store: store, Queue: queue, Local: local, Cloud: cloudPublisher{cloud},
		Photos: photos{dir}, Uploader: uploader{https}, NewID: ids.New, Logger: log, Key: key,
	})
	if err != nil {
		log.Error("cannot start the agent", "err", err)
		return 1
	}
	log.Info("starting", "site", cfg.App.Site, "cloud", cfg.Cloud, "broker", cfg.BrokerURL, "version", version)
	return daemon(cfg, app, local, cloud, log).Run(ctx)
}

// daemon runs the application between the hub's broker and the cloud's. At a stop,
// the application sends what is left for the hub, then the agent tells the cloud
// the site goes offline: a clean disconnection does not set off the Last Will, and
// the cloud would believe the site still connected.
func daemon(cfg agentconfig.Result, app *agentapp.App, local, cloud *service.Publisher, log *slog.Logger) service.Daemon {
	will := func() mqtt.Message {
		topic, payload := app.Will()
		return mqtt.Message{Topic: topic, Payload: payload, QoS: 1, Retain: true}
	}
	hub := service.Local(cfg, mqtt.Options{
		Subscriptions: service.Subscriptions(app.LocalSubscriptions()), Logger: log,
		Take: service.Take(app.HandleLocal),
	})
	toCloud := mqtt.Options{
		BrokerURL: cfg.Cloud.URLs[0], FallbackURLs: cfg.Cloud.URLs[1:], CAFile: cfg.Cloud.CAFile,
		ClientID: cfg.Cloud.ClientID, Username: cfg.Cloud.Username, Password: cfg.Cloud.Password,
		Subscriptions: service.Subscriptions(app.CloudSubscriptions()), Will: will, Spread: true, Logger: log.With("broker", "cloud"),
		Take:         service.Take(app.HandleCloud),
		OnLinkChange: app.LinkChanged,
	}
	return service.Daemon{
		Name: name, Log: log, Loops: []func(context.Context) error{app.Run}, Alive: app.Alive,
		Brokers: []service.Broker{{Options: hub, Publisher: local}, {Options: toCloud, Publisher: cloud}},
		BeforeClose: func(ctx context.Context) {
			w := will()
			if err := cloud.Publish(ctx, w.Topic, w.Payload, w.QoS, w.Retain); err != nil {
				log.Warn("could not tell the cloud the agent stops", "err", err)
			}
		},
	}
}
