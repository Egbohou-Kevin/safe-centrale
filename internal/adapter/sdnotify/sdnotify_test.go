package sdnotify

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// listen plays systemd: it collects what the program sends. The socket lives under
// /tmp, whose path is short enough for a Unix socket on every system.
func listen(t *testing.T) (string, func() []string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sdn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var mu sync.Mutex
	var got []string
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			mu.Lock()
			got = append(got, string(buf[:n]))
			mu.Unlock()
		}
	}()
	return path, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }
}

func count(msgs []string, want string) int {
	n := 0
	for _, m := range msgs {
		if m == want {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestOutsideSystemdNothingHappens(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	n := FromEnv()
	if n.Interval() != 0 || n.Ready() != nil || n.Watchdog() != nil || n.Stopping() != nil {
		t.Fatal("no socket: nothing to do")
	}
	var none *Notifier
	if none.Interval() != 0 || none.Ready() != nil {
		t.Fatal("a nil notifier does nothing")
	}
	done := make(chan struct{})
	go func() { n.Keepalive(context.Background(), nil, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("no watchdog: Keepalive returns at once")
	}
}

func TestTheEnvironment(t *testing.T) {
	path, _ := listen(t)
	t.Setenv("NOTIFY_SOCKET", path)
	t.Setenv("WATCHDOG_USEC", "30000000")
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()))
	if got := FromEnv().Interval(); got != 30*time.Second {
		t.Fatalf("%v", got)
	}
	t.Setenv("WATCHDOG_PID", "1") // the watchdog is for another process
	if FromEnv().Interval() != 0 {
		t.Fatal("not ours")
	}
	t.Setenv("WATCHDOG_PID", "")
	for _, bad := range []string{"", "0", "-5", "soon"} {
		t.Setenv("WATCHDOG_USEC", bad)
		if FromEnv().Interval() != 0 {
			t.Errorf("WATCHDOG_USEC=%q", bad)
		}
	}
}

func TestReadyAndStopping(t *testing.T) {
	path, got := listen(t)
	t.Setenv("NOTIFY_SOCKET", path)
	n := FromEnv()
	if err := n.Ready(); err != nil {
		t.Fatal(err)
	}
	if err := n.Stopping(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "two messages", func() bool { return len(got()) == 2 })
	if m := got(); m[0] != "READY=1" || m[1] != "STOPPING=1" {
		t.Fatalf("%q", m)
	}
	t.Setenv("NOTIFY_SOCKET", filepath.Join(t.TempDir(), "gone"))
	if FromEnv().Ready() == nil {
		t.Fatal("no systemd listening: an error")
	}
}

func TestKeepaliveFollowsTheProgramsHealth(t *testing.T) {
	path, got := listen(t)
	t.Setenv("NOTIFY_SOCKET", path)
	t.Setenv("WATCHDOG_USEC", "60000") // 60 ms
	n := FromEnv()

	var stuck atomic.Bool
	alive := func(ctx context.Context) error {
		if stuck.Load() {
			<-ctx.Done() // a stuck loop: the check gives up after half a period
			return ctx.Err()
		}
		return nil
	}
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Keepalive(ctx, alive, slog.New(slog.NewTextHandler(logs, nil))); close(done) }()

	waitFor(t, "pings", func() bool { return count(got(), "WATCHDOG=1") >= 3 })
	stuck.Store(true)
	time.Sleep(60 * time.Millisecond) // a check under way may still ping once
	before := count(got(), "WATCHDOG=1")
	time.Sleep(200 * time.Millisecond)
	if after := count(got(), "WATCHDOG=1"); after != before {
		t.Fatalf("%d pings while stuck: systemd would never restart it", after-before)
	}
	if strings.Count(logs.String(), "does not answer") != 1 {
		t.Fatalf("said once:\n%s", logs.String())
	}
	stuck.Store(false)
	waitFor(t, "pings again", func() bool { return count(got(), "WATCHDOG=1") > before })
	waitFor(t, "the recovery said", func() bool { return strings.Contains(logs.String(), "answers again") })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Keepalive did not stop")
	}

	// systemd gone: said, and the program goes on.
	logs2 := &syncBuffer{}
	t.Setenv("NOTIFY_SOCKET", filepath.Join(t.TempDir(), "gone"))
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	FromEnv().Keepalive(ctx2, func(context.Context) error { return nil }, slog.New(slog.NewTextHandler(logs2, nil)))
	if !strings.Contains(logs2.String(), "could not reach systemd") {
		t.Fatalf("%s", logs2.String())
	}
}
