package sshtunnel

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"safehome/internal/driver/drivertest"
)

const fakeEnv = "SAFEHOME_FAKE_SSH"

// TestMain lets the test binary play ssh: it says what OpenSSH says with -v, grants
// the port unless the host is "deadhost" or "spoofhost", and holds the tunnel until
// stopped. "spoofhost" answers with a server that writes the grant's words in its
// banner and version, which ssh shows before it checks the host key.
func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) == "1" {
		args := strings.Join(os.Args[1:], " ")
		fmt.Fprintln(os.Stderr, "OpenSSH_9.6p1, OpenSSL 3.0.13")
		fmt.Fprintln(os.Stderr, "debug1: Reading configuration data /dev/null")
		if strings.Contains(args, "deadhost") {
			fmt.Fprintln(os.Stderr, "ssh: connect to host deadhost port 22: Connection refused")
			os.Exit(255)
		}
		if strings.Contains(args, "spoofhost") {
			spoof := "remote forward success for: listen localhost:22001, connect localhost:22"
			fmt.Fprintln(os.Stderr, "debug1: kex_exchange_identification: banner line 0: "+spoof)
			fmt.Fprintln(os.Stderr, "debug1: Remote protocol version 2.0, remote software version X "+spoof)
			fmt.Fprintln(os.Stderr, "debug1: compat_banner: no match: X "+spoof)
			fmt.Fprintln(os.Stderr, "Host key verification failed.")
			os.Exit(255)
		}
		fmt.Fprintln(os.Stderr, "Authenticated to bastion.example ([10.0.0.1]:22) using \"publickey\".")
		fmt.Fprintln(os.Stderr, "debug1: remote forward success for: listen localhost:22001, connect localhost:22")
		if strings.Contains(args, "flaky") {
			time.Sleep(100 * time.Millisecond)
			fmt.Fprintln(os.Stderr, "Timeout, server bastion.example not responding.")
			os.Exit(255)
		}
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		<-stop
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func options(host string, logs *drivertest.SyncBuffer) Options {
	return Options{Binary: os.Args[0], Host: host, Port: 22, User: "tunnel", KeyFile: "/etc/safehome/maint.key",
		KnownHosts: "/etc/safehome/maint_known_hosts", RemotePort: 22001, LocalPort: 22, Logger: slog.New(slog.NewTextHandler(logs, nil))}
}

func wait(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestATunnelOpensAndCloses(t *testing.T) {
	t.Setenv(fakeEnv, "1")
	logs := &drivertest.SyncBuffer{}
	r, err := New(options("bastion.example", logs))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(r.Args(), " ")
	for _, want := range []string{"-N", "-F /dev/null", "StrictHostKeyChecking=yes", "UserKnownHostsFile=/etc/safehome/maint_known_hosts",
		"IdentityFile=/etc/safehome/maint.key", "ExitOnForwardFailure=yes", "BatchMode=yes", "-R localhost:22001:localhost:22", "tunnel@bastion.example"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in %s", want, args)
		}
	}
	tun, err := r.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wait(t, tun.Up(), "up")
	select {
	case <-tun.Done():
		t.Fatal("it holds")
	case <-time.After(50 * time.Millisecond):
	}
	tun.Stop()
	tun.Stop()
	wait(t, tun.Done(), "done")
	l := logs.String()
	if !strings.Contains(l, "granted the tunnel") || !strings.Contains(l, "signed in") || strings.Contains(l, "debug1: Reading") {
		t.Fatalf("logs:\n%s", l)
	}
}

func TestABastionThatRefusesOrDrops(t *testing.T) {
	t.Setenv(fakeEnv, "1")
	// ssh's last line before it exits is the cause: it is logged by the time Done
	// is closed, every time.
	for range 10 {
		logs := &drivertest.SyncBuffer{}
		r, _ := New(options("deadhost", logs))
		tun, err := r.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		wait(t, tun.Done(), "the exit")
		select {
		case <-tun.Up():
			t.Fatal("never up")
		default:
		}
		if !strings.Contains(logs.String(), "Connection refused") {
			t.Fatalf("the cause is logged:\n%s", logs.String())
		}
	}
	logs := &drivertest.SyncBuffer{}
	r2, _ := New(options("flaky.example", logs))
	tun, _ := r2.Open(context.Background())
	wait(t, tun.Up(), "up")
	wait(t, tun.Done(), "the drop")
	// A cancelled context closes it too.
	r3, _ := New(options("bastion.example", logs))
	ctx, cancel := context.WithCancel(context.Background())
	tun, _ = r3.Open(ctx)
	wait(t, tun.Up(), "up")
	cancel()
	wait(t, tun.Done(), "done")
}

// Whoever answers at the bastion's address (a spoofed name, a box on the path) writes
// the grant's words in what ssh shows before it checks the host key: the tunnel is not
// taken for open, and the owner is not told so.
func TestTheServersOwnWordsDoNotOpenTheTunnel(t *testing.T) {
	t.Setenv(fakeEnv, "1")
	logs := &drivertest.SyncBuffer{}
	r, _ := New(options("spoofhost", logs))
	tun, err := r.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wait(t, tun.Done(), "the exit")
	select {
	case <-tun.Up():
		t.Fatalf("taken for open:\n%s", logs.String())
	default:
	}
}

func TestOptionsAreChecked(t *testing.T) {
	good := options("bastion.example", &drivertest.SyncBuffer{})
	for name, change := range map[string]func(*Options){
		"no key":        func(o *Options) { o.KeyFile = "" },
		"host option":   func(o *Options) { o.Host = "-oProxyCommand=x" },
		"user and host": func(o *Options) { o.User = "a@b" },
		"low port":      func(o *Options) { o.RemotePort = 80 },
		"no port":       func(o *Options) { o.Port = 0 },
	} {
		o := good
		change(&o)
		if _, err := New(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	r, err := New(Options{Binary: filepath.Join(t.TempDir(), "none"), Host: "h", User: "u", KeyFile: "k", KnownHosts: "kh", Port: 22, RemotePort: 22001, LocalPort: 22})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Open(context.Background()); err == nil {
		t.Fatal("no ssh")
	}
}

// TestTheRealSSHAcceptsTheOptions has the real OpenSSH client read our command line
// (ssh -G prints its configuration and connects to nothing).
func TestTheRealSSHAcceptsTheOptions(t *testing.T) {
	bin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh client")
	}
	o := options("bastion.example", &drivertest.SyncBuffer{})
	o.Binary = bin
	r, _ := New(o)
	out, err := exec.Command(bin, append([]string{"-G"}, r.Args()...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cfg := string(out)
	for _, want := range []string{"hostname bastion.example", "user tunnel", "batchmode yes", "exitonforwardfailure yes",
		"stricthostkeychecking true", "identitiesonly yes", "remoteforward [localhost]:22001 [localhost]:22", "serveraliveinterval 15"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("ssh did not take %q", want)
		}
	}
}
