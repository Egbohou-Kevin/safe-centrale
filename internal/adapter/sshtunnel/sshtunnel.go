// Package sshtunnel opens the remote maintenance tunnel: the OpenSSH client connects
// out to our bastion and offers the hub's own SSH server there, on a port of the
// bastion's loopback (ssh -R). Nothing listens on the client's network.
//
// The bastion's host key is pinned (known_hosts file, strict checking), the hub
// signs in with its own key, and no user or system ssh configuration is read. The
// bastion lets this key do nothing but listen on its one port. Logging in to the hub
// through the tunnel then takes our administrators' key, which only the hub's
// authorized_keys knows.
package sshtunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options configures the tunnel.
type Options struct {
	Binary     string // the ssh client, /usr/bin/ssh
	Host       string // the bastion
	Port       int    // its SSH port
	User       string // the account the hubs sign in to
	KeyFile    string // the hub's private key
	KnownHosts string // the bastion's host key
	RemotePort int    // the port the hub gets on the bastion's loopback
	LocalPort  int    // the hub's SSH server, 22
	Logger     *slog.Logger
}

// Runner opens tunnels.
type Runner struct {
	o   Options
	log *slog.Logger
}

const (
	stopGrace   = 3 * time.Second
	outputGrace = time.Second // for ssh's output once it exited
)

// New checks the options.
func New(o Options) (*Runner, error) {
	switch {
	case o.Binary == "" || o.Host == "" || o.User == "" || o.KeyFile == "" || o.KnownHosts == "":
		return nil, errors.New("sshtunnel: Binary, Host, User, KeyFile and KnownHosts are required")
	case strings.ContainsAny(o.Host+o.User, " @:/\\") || strings.HasPrefix(o.Host, "-") || strings.HasPrefix(o.User, "-"):
		return nil, errors.New("sshtunnel: a plain host name and user are required")
	case o.Port < 1 || o.Port > 65535 || o.RemotePort < 1024 || o.RemotePort > 65535 || o.LocalPort < 1 || o.LocalPort > 65535:
		return nil, errors.New("sshtunnel: Port and LocalPort from 1, RemotePort from 1024, to 65535")
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{o: o, log: log}, nil
}

// Args returns the ssh command line, without the program.
func (r *Runner) Args() []string {
	o := r.o
	opts := []string{
		"BatchMode=yes",            // never a question: a password prompt would hang
		"ExitOnForwardFailure=yes", // no port on the bastion: no tunnel, and say so
		"ServerAliveInterval=15",   // a dead link is noticed in under a minute
		"ServerAliveCountMax=3",
		"ConnectTimeout=20",
		"StrictHostKeyChecking=yes", // only our bastion
		"UserKnownHostsFile=" + o.KnownHosts,
		"GlobalKnownHostsFile=/dev/null",
		"IdentitiesOnly=yes",
		"IdentityFile=" + o.KeyFile,
		"ForwardAgent=no",
		"ForwardX11=no",
		"PermitLocalCommand=no",
	}
	args := []string{"-v", "-N", "-T", "-F", "/dev/null"}
	for _, opt := range opts {
		args = append(args, "-o", opt)
	}
	return append(args,
		"-p", strconv.Itoa(o.Port),
		"-R", fmt.Sprintf("localhost:%d:localhost:%d", o.RemotePort, o.LocalPort),
		o.User+"@"+o.Host)
}

// Tunnel is one ssh process.
type Tunnel struct {
	cmd      *exec.Cmd
	up, done chan struct{}
	upOnce   sync.Once
	stopOnce sync.Once
}

// Open starts ssh and returns at once: Up is closed once the bastion granted the
// port, Done once ssh exited.
func (r *Runner) Open(ctx context.Context) (*Tunnel, error) {
	// A pipe of our own rather than StderrPipe: Wait must not close ssh's output
	// before its last line, which says why it stopped, is read.
	stderr, sink := io.Pipe()
	cmd := exec.Command(r.o.Binary, r.Args()...)
	cmd.SysProcAttr = sysProcAttr()
	cmd.Stdout, cmd.Stderr = io.Discard, sink
	cmd.WaitDelay = outputGrace
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("sshtunnel: %w", err)
	}
	t := &Tunnel{cmd: cmd, up: make(chan struct{}), done: make(chan struct{})}
	logs := make(chan struct{})
	go func() { t.read(stderr, r.log); close(logs) }()
	go func() {
		err := cmd.Wait()
		sink.Close()
		<-logs
		r.log.Info("ssh stopped", "err", err)
		close(t.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.done:
		}
	}()
	return t, nil
}

// read follows what ssh says (-v): the line that grants the port means the tunnel is
// up. Its warnings and errors go to the logs; its debug chatter does not. What the
// scanner cannot take (a line too long) is drained, so that ssh never blocks on it.
func (t *Tunnel) read(r io.Reader, log *slog.Logger) {
	defer io.Copy(io.Discard, r)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.Contains(line, "remote forward success for: listen"):
			log.Info("the bastion granted the tunnel", "line", line)
			t.upOnce.Do(func() { close(t.up) })
		case strings.HasPrefix(line, "Authenticated to "):
			log.Info("signed in to the bastion", "line", line)
		case line == "" || strings.HasPrefix(line, "debug"), strings.HasPrefix(line, "OpenSSH_"):
		default:
			log.Warn("ssh", "line", line)
		}
	}
}

// Up is closed once the bastion granted the port.
func (t *Tunnel) Up() <-chan struct{} { return t.up }

// Done is closed once ssh exited.
func (t *Tunnel) Done() <-chan struct{} { return t.done }

// Stop ends ssh (politely, then not) and waits for it.
func (t *Tunnel) Stop() {
	t.stopOnce.Do(func() {
		t.cmd.Process.Signal(os.Interrupt)
		select {
		case <-t.done:
		case <-time.After(stopGrace):
			t.cmd.Process.Kill()
			<-t.done
		}
	})
}
