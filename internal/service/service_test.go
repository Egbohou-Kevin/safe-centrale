package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/outbox"
)

// --- publisher ------------------------------------------------------------------

func TestPublisherIsUnavailableUntilItsBrokerAnswers(t *testing.T) {
	var p Publisher
	if err := p.Publish(context.Background(), "t", []byte("x"), 1, false); !errors.Is(err, outbox.ErrUnavailable) {
		t.Fatalf("before Attach: %v, want outbox.ErrUnavailable", err)
	}
	// Nothing listens on port 1: the client exists but is never connected.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := mqtt.Dial(ctx, mqtt.Options{BrokerURL: "mqtt://127.0.0.1:1", ClientID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	p.Attach(c)
	if err := p.Publish(context.Background(), "t", []byte("x"), 1, false); !errors.Is(err, outbox.ErrUnavailable) {
		t.Fatalf("broker down: %v, want outbox.ErrUnavailable", err)
	}
}

func TestSubscriptionsKeepFilterAndQoS(t *testing.T) {
	type appSubscription struct {
		Filter string
		QoS    byte
	}
	got := Subscriptions([]appSubscription{{"a/+", 1}, {"b", 0}})
	want := []mqtt.Subscription{{Filter: "a/+", QoS: 1}, {Filter: "b", QoS: 0}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// --- command line ---------------------------------------------------------------

func TestCLIParsesTheSharedCommandLine(t *testing.T) {
	cli := CLI{Program: "safehome-x", Version: "1.2", Usage: "usage: x\n", Extra: []string{"codes"}}
	cases := []struct {
		args     []string
		ok       bool
		exit     int
		cmd      Command
		out, err string // what stdout and stderr contain
	}{
		{args: nil, exit: 2, err: "usage: x"},
		{args: []string{"version"}, exit: 0, out: "safehome-x 1.2"},
		{args: []string{"explode"}, exit: 2, err: `unknown command "explode"`},
		{args: []string{"run"}, exit: 2, err: "run: -config is required"},
		{args: []string{"check", "-nope"}, exit: 2, err: "flag provided but not defined"},
		{args: []string{"run", "-config", "a.json"}, ok: true, cmd: Command{Name: "run", Config: "a.json", Args: []string{}}},
		{args: []string{"codes", "-config", "a.json", "set", "u1"}, ok: true,
			cmd: Command{Name: "codes", Config: "a.json", Args: []string{"set", "u1"}}},
	}
	for _, c := range cases {
		var stdout, stderr bytes.Buffer
		cmd, exit, ok := cli.Parse(c.args, &stdout, &stderr)
		if ok != c.ok || exit != c.exit {
			t.Errorf("%v: ok %v exit %d, want %v %d", c.args, ok, exit, c.ok, c.exit)
		}
		if cmd.Name != c.cmd.Name || cmd.Config != c.cmd.Config || !slices.Equal(cmd.Args, c.cmd.Args) {
			t.Errorf("%v: command %+v, want %+v", c.args, cmd, c.cmd)
		}
		if !strings.Contains(stdout.String(), c.out) || !strings.Contains(stderr.String(), c.err) {
			t.Errorf("%v: stdout %q stderr %q, want %q and %q", c.args, stdout.String(), stderr.String(), c.out, c.err)
		}
	}
}

// --- state ----------------------------------------------------------------------

func TestASecondCopyRefusesTheState(t *testing.T) {
	dir := t.TempDir()
	store, release, err := OpenState("hub", dir, "hub.pid", nil)
	if err != nil || store == nil {
		t.Fatalf("first copy: %v", err)
	}
	release()
	// The parent process (go test) is alive and holds the file.
	os.WriteFile(filepath.Join(dir, "hub.pid"), []byte(strconv.Itoa(os.Getppid())), 0o600)
	if _, _, err := OpenState("hub", dir, "hub.pid", nil); err == nil || !strings.Contains(err.Error(), "another hub is already using") {
		t.Fatalf("second copy: %v", err)
	}
	file := filepath.Join(dir, "a-file")
	os.WriteFile(file, nil, 0o600)
	if _, _, err := OpenState("hub", file, "hub.pid", nil); err == nil || !strings.Contains(err.Error(), "cannot open the state directory") {
		t.Fatalf("a file as the state directory: %v", err)
	}
}

// --- life cycle -----------------------------------------------------------------

// syncBuffer is a log the daemon writes while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// untilDone is a loop that runs until ctx ends, and counts how many have stopped.
func untilDone(stopped *atomic.Int32) func(context.Context) error {
	return func(ctx context.Context) error {
		<-ctx.Done()
		stopped.Add(1)
		return nil
	}
}

func alive(context.Context) error { return nil }

func newDaemon(logs *syncBuffer, loops ...func(context.Context) error) Daemon {
	return Daemon{Name: "test service", Log: Logger(logs, 0), Loops: loops, Alive: alive}
}

// runAsync runs the daemon and gives its exit code.
func runAsync(ctx context.Context, d Daemon) <-chan int {
	done := make(chan int, 1)
	go func() { done <- d.Run(ctx) }()
	return done
}

func exitCode(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop")
		return -1
	}
}

func TestDaemonStopsTheLoopsBeforeItsLastWords(t *testing.T) {
	var logs syncBuffer
	var stopped atomic.Int32
	d := newDaemon(&logs, untilDone(&stopped), untilDone(&stopped))
	pub := &Publisher{}
	d.Brokers = []Broker{{Options: mqtt.Options{BrokerURL: "mqtt://127.0.0.1:1", ClientID: "t"}, Publisher: pub}}
	var atLastWords int32 = -1
	d.BeforeClose = func(context.Context) { atLastWords = stopped.Load() }

	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, d)
	deadline := time.Now().Add(5 * time.Second)
	for pub.client.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if pub.client.Load() == nil {
		t.Fatal("the publisher was never given its client")
	}
	cancel()
	if code := exitCode(t, done); code != 0 {
		t.Fatalf("exit %d\n%s", code, logs.String())
	}
	if atLastWords != 2 {
		t.Fatalf("%d loop(s) had stopped at the last words, want 2", atLastWords)
	}
	if !strings.Contains(logs.String(), "msg=stopping") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}

func TestDaemonRestartIsACleanStop(t *testing.T) {
	var logs syncBuffer
	var stopped atomic.Int32
	d := newDaemon(&logs, untilDone(&stopped))
	d.Restart = NewRestart()
	d.Restart.Ask()
	d.Restart.Ask() // asking twice never blocks
	if code := exitCode(t, runAsync(context.Background(), d)); code != 0 || stopped.Load() != 1 {
		t.Fatalf("exit %d, %d loop(s) stopped\n%s", code, stopped.Load(), logs.String())
	}
	if !strings.Contains(logs.String(), "to start again with the new configuration") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}

func TestDaemonGivesUpWhenALoopStopsOnItsOwn(t *testing.T) {
	var logs syncBuffer
	var stopped atomic.Int32
	broken := func(context.Context) error { return errors.New("the loop broke") }
	d := newDaemon(&logs, untilDone(&stopped), broken)
	if code := exitCode(t, runAsync(context.Background(), d)); code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, logs.String())
	}
	if l := logs.String(); !strings.Contains(l, "the test service stopped unexpectedly") || !strings.Contains(l, "the loop broke") {
		t.Fatalf("logs:\n%s", l)
	}
}

// A loop that ends with ctx is not a loop that gave up: systemd must see a clean stop
// however the stop and the loop's end race.
func TestDaemonStopIsCleanWhenTheLoopsEndFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 200 {
		var logs syncBuffer
		quick := func(ctx context.Context) error { return ctx.Err() }
		if code := newDaemon(&logs, quick).Run(ctx); code != 0 {
			t.Fatalf("exit %d, want 0\n%s", code, logs.String())
		}
	}
}

func TestDaemonThatCannotDialStopsItsLoops(t *testing.T) {
	var logs syncBuffer
	var stopped atomic.Int32
	d := newDaemon(&logs, untilDone(&stopped))
	d.Brokers = []Broker{{Options: mqtt.Options{BrokerURL: "", ClientID: "t"}}}
	if code := d.Run(context.Background()); code != 1 || stopped.Load() != 1 {
		t.Fatalf("exit %d, %d loop(s) stopped, want 1 and 1\n%s", code, stopped.Load(), logs.String())
	}
	if !strings.Contains(logs.String(), "cannot start the MQTT client") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}
