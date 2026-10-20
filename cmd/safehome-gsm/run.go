package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"safehome/internal/adapter/atmodem"
	"safehome/internal/adapter/filestore"
	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/pidfile"
	"safehome/internal/adapter/sdnotify"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/gateway"
	"safehome/internal/gsmconfig"
	"safehome/internal/outbox"
	"safehome/internal/siteconfig"
)

const pidName = "gsm.pid"

const usage = `usage:
  safehome-gsm run   -config FILE   run the GSM gateway
  safehome-gsm check -config FILE   check the configuration and exit
  safehome-gsm version
`

// openPort opens a modem's serial port. Tests replace it with a simulated modem.
var openPort = func(p gsmconfig.Port) (io.ReadWriteCloser, error) {
	if p.Configure {
		// Raw mode at the right speed. The standard library has no termios: stty does it.
		flagDev := "-F"
		if runtime.GOOS == "darwin" {
			flagDev = "-f"
		}
		out, err := exec.Command("stty", flagDev, p.Path, strconv.Itoa(p.Baud), "raw", "-echo").CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("configuring %s: %v: %s", p.Path, err, out)
		}
	}
	return os.OpenFile(p.Path, os.O_RDWR|syscall.O_NOCTTY, 0)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Fprintln(stdout, "safehome-gsm", version)
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
	cfg, err := gsmconfig.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}
	if cmd == "check" {
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
		return 0
	}
	return runGateway(ctx, cfg, stderr)
}

// brokerPublisher gives the gateway a publisher before the MQTT client exists.
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

func runGateway(ctx context.Context, cfg gsmconfig.Result, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	store, err := filestore.Open(cfg.StateDir, log)
	if err != nil {
		log.Error("cannot open the state directory", "dir", cfg.StateDir, "err", err)
		return 1
	}
	release, err := pidfile.Lock(cfg.StateDir, pidName)
	if err != nil {
		log.Error("another gateway is already using this state directory", "dir", cfg.StateDir, "err", err)
		return 1
	}
	defer release()

	// The modems call the gateway back (incoming SMS, delivery reports), and the
	// gateway needs the modems: the pointer is set once the gateway exists.
	var gw atomic.Pointer[gateway.Gateway]
	lines := map[string]gateway.Modem{}
	var modems []*lazyModem
	for _, l := range cfg.Gateway.GSM.Lines {
		port := cfg.Ports[l.ID]
		m := &lazyModem{opts: atmodem.Options{
			Open:   func() (io.ReadWriteCloser, error) { return openPort(port) },
			Logger: log.With("line", l.ID),
			OnSMS: func(from, text string) {
				if g := gw.Load(); g != nil {
					g.Incoming(l.ID, from, text)
				}
			},
			OnDeliveryReport: func(ref string, ok bool) {
				if g := gw.Load(); g != nil {
					g.Delivered(l.ID, ref, ok)
				}
			},
		}}
		modems = append(modems, m)
		lines[l.ID] = m
	}
	defer func() {
		for _, m := range modems {
			m.close()
		}
	}()

	ids := contract.NewIDGenerator()
	pub := &brokerPublisher{}
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

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { g.Run(runCtx); close(done) }()

	// The contacts' numbers may come from the cloud: once the hub runs with a new
	// configuration, the gateway restarts to load it (systemd starts it again).
	applied := contract.ConfigAppliedTopic(cfg.Gateway.Site)
	restart := make(chan struct{}, 1)
	subs := []mqtt.Subscription{{Filter: applied, QoS: 1}}
	for _, s := range g.Subscriptions() {
		subs = append(subs, mqtt.Subscription{Filter: s.Filter, QoS: s.QoS})
	}
	client, err := mqtt.Dial(runCtx, mqtt.Options{
		BrokerURL: cfg.BrokerURL, ClientID: cfg.ClientID, Subscriptions: subs, Logger: log,
		OnMessage: func(m mqtt.Message) {
			if m.Topic == applied {
				if v, ok := siteconfig.Changed(cfg.StateDir, cfg.Gateway.Site, cfg.SiteVersion, m.Payload); ok {
					log.Info("restarting to load the configuration from the cloud", "site_config", v)
					select {
					case restart <- struct{}{}:
					default:
					}
				}
				return
			}
			g.HandleMessage(gateway.Message{Topic: m.Topic, Payload: m.Payload})
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
	// gateway stops saying so and systemd restarts it (WatchdogSec).
	notify := sdnotify.FromEnv()
	if err := notify.Ready(); err != nil {
		log.Warn("could not tell systemd the gateway is ready", "err", err)
	}
	go notify.Keepalive(runCtx, g.Alive, log)

	select {
	case <-ctx.Done():
		log.Info("stopping")
	case <-restart:
	case <-done:
		log.Error("the gateway stopped unexpectedly")
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

// lazyModem opens its modem on first use, and again after a failed start: a modem
// missing at boot must not stop the gateway, whose line checks will keep trying.
// It also translates the adapter's errors into the gateway's.
type lazyModem struct {
	mu   sync.Mutex
	opts atmodem.Options
	m    *atmodem.Modem
}

func (l *lazyModem) get(ctx context.Context) (*atmodem.Modem, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		m, err := atmodem.Open(ctx, l.opts)
		if err != nil {
			return nil, err
		}
		l.m = m
	}
	return l.m, nil
}

func (l *lazyModem) SendSMS(ctx context.Context, number, text string) (string, error) {
	m, err := l.get(ctx)
	if err != nil {
		return "", translate(err)
	}
	ref, err := m.SendSMS(ctx, number, text)
	return ref, translate(err)
}

func (l *lazyModem) Call(ctx context.Context, number string, ringFor time.Duration) error {
	m, err := l.get(ctx)
	if err != nil {
		return translate(err)
	}
	return translate(m.Call(ctx, number, ringFor))
}

func (l *lazyModem) Check(ctx context.Context) (gateway.LineInfo, error) {
	m, err := l.get(ctx)
	if err != nil {
		return gateway.LineInfo{}, translate(err)
	}
	info, err := m.Check(ctx)
	return gateway.LineInfo{Registered: info.Registered, Signal: info.Signal, Operator: info.Operator}, translate(err)
}

func (l *lazyModem) Restart(ctx context.Context) error {
	l.mu.Lock()
	m := l.m
	l.mu.Unlock()
	if m == nil {
		_, err := l.get(ctx) // never opened: opening is the restart
		return translate(err)
	}
	return translate(m.Restart(ctx))
}

func (l *lazyModem) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m != nil {
		l.m.Close()
	}
}

var errorMap = []struct{ from, to error }{
	{atmodem.ErrNoNetwork, gateway.ErrNoNetwork},
	{atmodem.ErrNoCredit, gateway.ErrNoCredit},
	{atmodem.ErrBusy, gateway.ErrBusy},
	{atmodem.ErrUnreachable, gateway.ErrUnreachable},
	{atmodem.ErrTimeout, gateway.ErrTimeout},
}

func translate(err error) error {
	if err == nil {
		return nil
	}
	if i := slices.IndexFunc(errorMap, func(e struct{ from, to error }) bool { return errors.Is(err, e.from) }); i >= 0 {
		return fmt.Errorf("%w: %v", errorMap[i].to, err)
	}
	return err
}
