package main

import (
	"context"
	"crypto/rand"
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
	"safehome/internal/adapter/pidfile"
	"safehome/internal/adapter/sdnotify"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/outbox"
	"safehome/internal/zigbeeapp"
	"safehome/internal/zigbeeconfig"
)

const pidName = "zigbee.pid"

const usage = `usage:
  safehome-zigbee run   -config FILE   run the Zigbee adapter
  safehome-zigbee check -config FILE   check the configuration and the profiles, and exit
  safehome-zigbee version
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Fprintln(stdout, "safehome-zigbee", version)
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
	cfg, err := zigbeeconfig.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}
	if cmd == "check" {
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

// brokerPublisher gives the adapter a publisher before the MQTT client exists.
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
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	store, err := filestore.Open(cfg.StateDir, log)
	if err != nil {
		log.Error("cannot open the state directory", "dir", cfg.StateDir, "err", err)
		return 1
	}
	release, err := pidfile.Lock(cfg.StateDir, pidName)
	if err != nil {
		log.Error("another Zigbee adapter is already using this state directory", "dir", cfg.StateDir, "err", err)
		return 1
	}
	defer release()

	ids := contract.NewIDGenerator()
	pub := &brokerPublisher{}
	app, err := zigbeeapp.New(cfg.App, zigbeeapp.Deps{
		Clock: driver.SystemClock(), Store: store, Publisher: pub, NewID: ids.New, NewDeviceID: newDeviceID, Logger: log,
	})
	if err != nil {
		log.Error("cannot start the adapter", "err", err)
		return 1
	}
	log.Info("starting", "site", cfg.App.Site, "profiles", len(cfg.App.Zigbee.Profiles), "broker", cfg.BrokerURL, "version", version)

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
			app.HandleMessage(zigbeeapp.Message{Topic: m.Topic, Payload: m.Payload, Retain: m.Retain})
		},
	})
	if err != nil {
		log.Error("cannot start the MQTT client", "err", err)
		cancel()
		<-done
		return 1
	}
	pub.client.Store(client)

	// systemd: started, then "still working" for as long as the loop answers. A stuck
	// adapter stops saying so and systemd restarts it (WatchdogSec).
	notify := sdnotify.FromEnv()
	if err := notify.Ready(); err != nil {
		log.Warn("could not tell systemd the adapter is ready", "err", err)
	}
	go notify.Keepalive(runCtx, app.Alive, log)

	select {
	case <-ctx.Done():
		log.Info("stopping")
		notify.Stopping()
		cancel()
		<-done
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		client.Close(closeCtx)
		stop()
		return 0
	case <-done:
		log.Error("the adapter stopped unexpectedly")
		return 1
	}
}
