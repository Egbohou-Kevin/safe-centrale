package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"

	"safehome/internal/adapter/filestore"
	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/pidfile"
	"safehome/internal/adapter/sdnotify"
	"safehome/internal/inbox"
)

// closeWait bounds the clean disconnection from the brokers at a stop.
const closeWait = 5 * time.Second

// Lock takes the program's pid file in its state directory: a second copy of the
// program on the same files would believe it owns them too, so it refuses to start.
// name says what the program is ("hub"); release gives the file back.
func Lock(name, dir, pidName string) (release func(), err error) {
	release, err = pidfile.Lock(dir, pidName)
	if err != nil {
		return nil, fmt.Errorf("another %s is already using the state directory %s: %w", name, dir, err)
	}
	return release, nil
}

// OpenState opens the program's state directory, then locks it (see Lock).
func OpenState(name, dir, pidName string, log *slog.Logger) (*filestore.Store, func(), error) {
	store, err := filestore.Open(dir, log)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot open the state directory %s: %w", dir, err)
	}
	release, err := Lock(name, dir, pidName)
	if err != nil {
		return nil, nil, err
	}
	return store, release, nil
}

// ErrNotRunning: no process holds the pid file.
var ErrNotRunning = errors.New("not running")

// Reload tells the program whose pid file is pidName in dir to read its files again
// (SIGHUP; see the hub's codes). It returns the process told, or ErrNotRunning. Only
// the program that holds its pid file is told: the pid left in the file of a program
// that is not running may now be another program's, which SIGHUP would stop.
func Reload(dir, pidName string) (pid int, err error) {
	pid, ok := pidfile.Held(dir, pidName)
	if !ok {
		return 0, ErrNotRunning
	}
	p, err := os.FindProcess(pid)
	if err == nil {
		err = p.Signal(syscall.SIGHUP)
	}
	return pid, err
}

// Broker is one MQTT client of a program, and the publisher that sends through it.
type Broker struct {
	Options   mqtt.Options
	Publisher *Publisher // attached once the client is dialed; nil if the program only listens
}

// Restart asks a running program to stop cleanly, so that systemd starts it again:
// to load a new configuration. Asking twice is asking once.
type Restart chan struct{}

// NewRestart returns a Restart to give to Daemon.
func NewRestart() Restart { return make(Restart, 1) }

// Ask asks for the restart. It never blocks.
func (r Restart) Ask() {
	select {
	case r <- struct{}{}:
	default:
	}
}

// Watch returns the handler of a program that restarts to load a new configuration:
// a message on topic goes to changed, and the program restarts when changed reports
// a new version; any other message goes to handle.
func (r Restart) Watch(topic string, changed func(payload []byte) (version int, ok bool), handle func(inbox.Message), log *slog.Logger) func(inbox.Message) {
	return func(m inbox.Message) {
		if m.Topic != topic {
			handle(m)
			return
		}
		if v, ok := changed(m.Payload); ok {
			log.Info("restarting to load the configuration from the cloud", "site_config", v)
			r.Ask()
		}
	}
}

// Daemon is the life cycle of a program under systemd. Run starts the application's
// loops, connects to the brokers, tells systemd the program is ready and keeps
// telling it the program works while Alive says so. At the end of ctx, or at a
// restart, it stops the loops first, so that they send their last messages, then
// disconnects.
type Daemon struct {
	Name  string // what the program is, in its logs: "hub", "GSM gateway"
	Log   *slog.Logger
	Loops []func(context.Context) error // each runs until ctx ends; one that returns sooner stops the program
	Alive func(context.Context) error   // whether the loops still answer (systemd's watchdog)

	Brokers []Broker // dialed in this order once the loops run, closed in the reverse order
	Restart Restart  // nil when the program never restarts itself

	// BeforeClose, when set, runs once the loops have stopped and before the clients
	// close: the agent says there that the site goes offline.
	BeforeClose func(ctx context.Context)
}

// Run runs the program until ctx ends, and returns its exit code: 0 after a clean
// stop (asked, or a restart), 1 when it could not start or a loop stopped on its own.
func (d Daemon) Run(ctx context.Context) int {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped, wait := d.start(runCtx)

	// The clients outlive the loops, which send their last messages as they stop.
	clientsCtx, closeClients := context.WithCancel(context.Background())
	defer closeClients()
	clients, err := d.dial(clientsCtx)
	if err != nil {
		d.Log.Error("cannot start the MQTT client", "err", err)
		cancel()
		wait()
		return 1
	}

	// systemd: started, then "still working" for as long as the loops answer. A
	// program stuck in a loop stops saying so and systemd restarts it (WatchdogSec).
	notify := sdnotify.FromEnv()
	if err := notify.Ready(); err != nil {
		d.Log.Warn("could not tell systemd the "+d.Name+" is ready", "err", err)
	}
	go notify.Keepalive(runCtx, d.Alive, d.Log)

	select {
	case <-ctx.Done():
		d.Log.Info("stopping")
	case <-d.Restart:
		d.Log.Info("stopping, to start again with the new configuration")
	case err := <-stopped:
		if ctx.Err() == nil { // not the loop ending with ctx: it gave up
			d.Log.Error("the "+d.Name+" stopped unexpectedly", "err", err)
			return 1
		}
		d.Log.Info("stopping")
	}
	notify.Stopping()
	cancel()
	wait()
	d.close(clients)
	return 0
}

// start runs each loop in its own goroutine. stopped gives the result of the first
// loop to return; wait returns once they all have.
func (d Daemon) start(ctx context.Context) (stopped <-chan error, wait func()) {
	results := make(chan error, len(d.Loops))
	var wg sync.WaitGroup
	for _, loop := range d.Loops {
		wg.Go(func() { results <- loop(ctx) })
	}
	return results, wg.Wait
}

// dial connects to each broker and attaches its publisher.
func (d Daemon) dial(ctx context.Context) ([]*mqtt.Client, error) {
	var clients []*mqtt.Client
	for _, b := range d.Brokers {
		c, err := mqtt.Dial(ctx, b.Options)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", b.Options.BrokerURL, err)
		}
		if b.Publisher != nil {
			b.Publisher.Attach(c)
		}
		clients = append(clients, c)
	}
	return clients, nil
}

// close says the last words, then disconnects from each broker cleanly.
func (d Daemon) close(clients []*mqtt.Client) {
	ctx, stop := context.WithTimeout(context.Background(), closeWait)
	defer stop()
	if d.BeforeClose != nil {
		d.BeforeClose(ctx)
	}
	for i := len(clients) - 1; i >= 0; i-- {
		clients[i].Close(ctx)
	}
}
