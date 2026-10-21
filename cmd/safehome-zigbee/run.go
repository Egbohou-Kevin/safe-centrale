package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"strings"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/service"
	"safehome/internal/zigbeeapp"
	"safehome/internal/zigbeeconfig"
)

const (
	name    = "Zigbee adapter" // in the logs
	pidName = "zigbee.pid"
)

var commandLine = service.CLI{Program: "safehome-zigbee", Version: version, Usage: `usage:
  safehome-zigbee run   -config FILE   run the Zigbee adapter
  safehome-zigbee check -config FILE   check the configuration and the profiles, and exit
  safehome-zigbee version
`}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd, exit, ok := commandLine.Parse(args, stdout, stderr)
	if !ok {
		return exit
	}
	cfg, err := zigbeeconfig.Load(cmd.Config)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}
	if cmd.Name == "check" {
		var names []string
		for _, p := range cfg.App.Zigbee.Profiles {
			names = append(names, p.Name)
		}
		fmt.Fprintf(stdout, "configuration OK: site %s, %d profile(s) (%s), Zigbee2MQTT on %s, state in %s\n",
			cfg.App.Site, len(names), strings.Join(names, ", "), cfg.App.Base, cfg.StateDir)
		return 0
	}
	return runAdapter(ctx, cfg, stderr)
}

// idAlphabet leaves out the letters and digits that read alike (l, 1, o, 0, i).
const idAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// newDeviceID proposes an id for a new device, such as dev_k7p2. The core refuses
// one already given and asks again.
func newDeviceID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "" // the core asks again, then refuses the device
	}
	out := []byte("dev_")
	for _, c := range b {
		out = append(out, idAlphabet[int(c)%len(idAlphabet)])
	}
	return string(out)
}

func runAdapter(ctx context.Context, cfg zigbeeconfig.Result, stderr io.Writer) int {
	log := service.Logger(stderr, cfg.LogLevel)
	store, release, err := service.OpenState(name, cfg.StateDir, pidName, log)
	if err != nil {
		log.Error("cannot start", "err", err)
		return 1
	}
	defer release()

	ids := contract.NewIDGenerator()
	pub := &service.Publisher{}
	app, err := zigbeeapp.New(cfg.App, zigbeeapp.Deps{
		Clock: driver.SystemClock(), Store: store, Publisher: pub, NewID: ids.New, NewDeviceID: newDeviceID, Logger: log,
	})
	if err != nil {
		log.Error("cannot start the adapter", "err", err)
		return 1
	}
	log.Info("starting", "site", cfg.App.Site, "profiles", len(cfg.App.Zigbee.Profiles), "broker", cfg.BrokerURL, "version", version)

	return service.Daemon{
		Name: name, Log: log, Loops: []func(context.Context) error{app.Run}, Alive: app.Alive,
		Brokers: []service.Broker{{Publisher: pub, Options: mqtt.Options{
			BrokerURL: cfg.BrokerURL, ClientID: cfg.ClientID, Subscriptions: service.Subscriptions(app.Subscriptions()), Logger: log,
			OnMessage: service.Deliver(app.HandleMessage),
		}}},
	}.Run(ctx)
}
