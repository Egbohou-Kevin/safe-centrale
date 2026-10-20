package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"safehome/internal/access"
	"safehome/internal/adapter/filestore"
	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/pidfile"
	"safehome/internal/adapter/sdnotify"
	"safehome/internal/adapter/sysclock"
	"safehome/internal/configapp"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/hub"
	"safehome/internal/hubconfig"
	"safehome/internal/siteconfig"
)

// pidName is the hub's pid file in its state directory.
const pidName = "hub.pid"

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

// run is main without the process: it takes everything it needs, so tests can run
// it. It returns the exit code.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Fprintln(stdout, "safehome-hub", version)
		return 0
	case "run", "check", "codes":
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
	cfg, err := hubconfig.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "configuration error:", err)
		return 1
	}

	switch cmd {
	case "check":
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
		return 0
	case "codes":
		return codes(cfg, fs.Args(), stdin, stdout, stderr)
	}
	return runHub(ctx, cfg, *configPath, stderr)
}

func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// brokerPublisher gives the hub a Publisher before the MQTT client exists: the
// client needs the hub (for its messages) and the hub needs the client (to send).
type brokerPublisher struct{ client atomic.Pointer[mqtt.Client] }

func (b *brokerPublisher) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	c := b.client.Load()
	if c == nil {
		return hub.ErrUnavailable
	}
	err := c.Publish(ctx, mqtt.Message{Topic: topic, Payload: payload, QoS: qos, Retain: retain})
	if errors.Is(err, mqtt.ErrDown) {
		return hub.ErrUnavailable
	}
	return err
}

func runHub(ctx context.Context, cfg hubconfig.Result, configPath string, stderr io.Writer) int {
	log := newLogger(stderr, cfg.LogLevel)

	// SIGHUP means "the installer's tool changed the codes, read them again". It is
	// caught before the pid file appears: a signal sent in between would otherwise
	// kill the hub, whose default reaction to SIGHUP is to exit.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	store, err := filestore.Open(cfg.StateDir, log)
	if err != nil {
		log.Error("cannot open the state directory", "dir", cfg.StateDir, "err", err)
		return 1
	}
	release, err := pidfile.Lock(cfg.StateDir, pidName)
	if err != nil {
		log.Error("another hub is already using this state directory", "dir", cfg.StateDir, "err", err)
		return 1
	}
	defer release()

	if cfg.DevicesErr != nil {
		log.Error("the zones and sirens set by the installer's tools are left aside: the hub runs on its own file", "err", cfg.DevicesErr)
	}

	ids := contract.NewIDGenerator()
	pub := &brokerPublisher{}
	var conf *configapp.App // built below; the hub calls it only once running
	h, err := hub.New(cfg.Hub, hub.Deps{
		Clock: driver.SystemClock(), Store: store, Publisher: pub, NewID: ids.New, Logger: log,
		ClockSynced: sysclock.Synced,
		// The installer changed the zones or the sirens (the local console, the
		// command-line tool): restart to load them, as for a configuration from the
		// cloud.
		DevicesChanged:   func() bool { return hubconfig.DevicesSum(cfg.StateDir) != cfg.DevicesSum },
		RestartWhenQuiet: func() { conf.RestartWhenQuiet() },
	})
	if err != nil {
		log.Error("cannot start the hub", "err", err)
		return 1
	}
	log.Info("starting", "site", cfg.Hub.Site, "broker", cfg.BrokerURL, "version", version)

	// The configuration set from the cloud: checked against this file, kept in the
	// state folder, applied by restarting (systemd starts the hub again).
	restart := make(chan struct{}, 1)
	ccfg := configapp.DefaultConfig(cfg.Hub.Site, cfg.SiteVersion)
	if cfg.SiteErr != nil {
		ccfg.Running = 0
		if cfg.SiteVersion > 0 {
			ccfg.Refused, ccfg.RefusedBecause = cfg.SiteVersion, cfg.SiteErr.Error()
		} else {
			log.Error("the configuration from the cloud cannot be read: the hub runs on its own file", "err", cfg.SiteErr)
		}
	}
	conf, err = configapp.New(ccfg, configapp.Deps{
		Clock: driver.SystemClock(), Store: store, Publisher: pub, NewID: ids.New, Logger: log,
		Check: func(d contract.ConfigSetData) error {
			_, err := hubconfig.WithSiteConfig(configPath, d)
			return err
		},
		Save:    func(d contract.ConfigSetData) error { return siteconfig.Write(cfg.StateDir, d) },
		Restart: func() { signalOnce(restart) },
	})
	if err != nil {
		log.Error("cannot start the configuration from the cloud", "err", err)
		return 1
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	hubDone, confDone := make(chan struct{}), make(chan struct{})
	go func() { h.Run(runCtx); close(hubDone) }()
	go func() { conf.Run(runCtx); close(confDone) }()

	var subs []mqtt.Subscription
	for _, s := range h.Subscriptions() {
		subs = append(subs, mqtt.Subscription{Filter: s.Filter, QoS: s.QoS})
	}
	confTopics := map[string]bool{}
	for _, s := range conf.Subscriptions() {
		subs = append(subs, mqtt.Subscription{Filter: s.Filter, QoS: s.QoS})
		confTopics[s.Filter] = true
	}
	client, err := mqtt.Dial(runCtx, mqtt.Options{
		BrokerURL: cfg.BrokerURL, ClientID: cfg.ClientID, Subscriptions: subs, Logger: log,
		OnSubscribed: h.LinkUp,
		OnMessage: func(m mqtt.Message) {
			if confTopics[m.Topic] { // exact topics, no wildcard
				conf.HandleMessage(configapp.Message{Topic: m.Topic, Payload: m.Payload})
				return
			}
			h.HandleMessage(hub.Message{Topic: m.Topic, Payload: m.Payload, Retain: m.Retain})
		},
	})
	if err != nil {
		log.Error("cannot start the MQTT client", "err", err)
		cancel()
		<-hubDone
		<-confDone
		return 1
	}
	pub.client.Store(client)

	// systemd: started, then "still working" for as long as the loops answer. A hub
	// stuck in a loop stops saying so and systemd restarts it (WatchdogSec).
	notify := sdnotify.FromEnv()
	if err := notify.Ready(); err != nil {
		log.Warn("could not tell systemd the hub is ready", "err", err)
	}
	go notify.Keepalive(runCtx, h.Alive, log)

	for {
		select {
		case <-hup:
			if err := h.ReloadCodes(); err != nil {
				log.Error("reloading the access codes failed, the previous ones stay in use", "err", err)
			}
		case <-ctx.Done():
			log.Info("stopping")
			return stopHub(notify, cancel, hubDone, confDone, client)
		case <-restart:
			log.Info("stopping, to start again with the new configuration")
			return stopHub(notify, cancel, hubDone, confDone, client)
		case <-hubDone:
			log.Error("the hub stopped unexpectedly")
			return 1
		}
	}
}

// stopHub stops the hub cleanly: it sends what is left before returning.
func stopHub(notify *sdnotify.Notifier, cancel context.CancelFunc, hubDone, confDone <-chan struct{}, client *mqtt.Client) int {
	notify.Stopping()
	cancel()
	<-hubDone
	<-confDone
	closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	client.Close(closeCtx)
	stop()
	return 0
}

func signalOnce(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// --- codes ----------------------------------------------------------------------------

func codes(cfg hubconfig.Result, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	store, err := filestore.Open(cfg.StateDir, nil)
	if err != nil {
		fmt.Fprintln(stderr, "cannot open the state directory:", err)
		return 1
	}
	people, err := hub.OpenCodes(cfg.Hub.Access, store)
	if err != nil {
		fmt.Fprintln(stderr, "cannot read the access codes:", err)
		return 1
	}
	installer, err := hub.OpenInstallerCode(cfg.Hub.Access, store)
	if err != nil {
		fmt.Fprintln(stderr, "cannot read the installer code:", err)
		return 1
	}
	// reg is the registry of the user named, other the one its code must not match:
	// one code opens one thing.
	reg, other := people, installer
	if len(args) == 2 && args[1] == hub.InstallerUser {
		reg, other = installer, people
	}

	switch args[0] {
	case "list":
		users := append(people.Users(), installer.Users()...)
		if len(users) == 0 {
			fmt.Fprintln(stdout, "no access code is set")
		}
		for _, u := range users {
			fmt.Fprintln(stdout, u)
		}
		return 0

	case "set":
		if len(args) != 2 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		code, err := readCode(stdin)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if _, taken := other.Verify(code); taken {
			fmt.Fprintln(stderr, "the code was not set:", describe(access.ErrCodeTaken))
			return 1
		}
		if err := reg.Set(args[1], code); err != nil {
			fmt.Fprintln(stderr, "the code was not set:", describe(err))
			return 1
		}
		fmt.Fprintf(stdout, "code set for %s\n", args[1])
		notifyRunningHub(cfg.StateDir, stdout)
		return 0

	case "remove":
		if len(args) != 2 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		if err := reg.Remove(args[1]); err != nil {
			fmt.Fprintln(stderr, "nothing was removed:", describe(err))
			return 1
		}
		fmt.Fprintf(stdout, "code removed for %s\n", args[1])
		notifyRunningHub(cfg.StateDir, stdout)
		return 0
	}
	fmt.Fprintf(stderr, "unknown codes command %q\n%s", args[0], usage)
	return 2
}

// readCode reads the first line of the input. It is never echoed back, and never
// part of a message from this program.
func readCode(in io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(in, 256)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading the code: %w", err)
	}
	code := strings.TrimSpace(line)
	if code == "" {
		return "", errors.New("no code given: write it on the standard input")
	}
	return code, nil
}

func describe(err error) string {
	switch {
	case errors.Is(err, access.ErrInvalidCode):
		return "a code is 4 to 8 digits"
	case errors.Is(err, access.ErrWeakCode):
		return "too easy to guess (no repeated digit, no straight run of digits)"
	case errors.Is(err, access.ErrCodeTaken):
		return "another person already has this code"
	case errors.Is(err, access.ErrTooManyUsers):
		return "the site already has the maximum number of codes: remove one first"
	case errors.Is(err, access.ErrInvalidUser):
		return "invalid user identifier"
	case errors.Is(err, access.ErrNoSuchUser):
		return "no such user"
	}
	return err.Error()
}

// notifyRunningHub asks a running hub to read the codes again.
func notifyRunningHub(dir string, stdout io.Writer) {
	pid, ok := pidfile.Read(dir, pidName)
	if !ok || !pidfile.Alive(pid) {
		fmt.Fprintln(stdout, "no hub is running: the new codes apply at its next start")
		return
	}
	p, err := os.FindProcess(pid)
	if err == nil && p.Signal(syscall.SIGHUP) == nil {
		fmt.Fprintf(stdout, "the running hub (process %d) was told to reload the codes\n", pid)
		return
	}
	fmt.Fprintf(stdout, "could not signal process %d: restart the hub to apply the codes\n", pid)
}
