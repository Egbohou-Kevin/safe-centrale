// Package sdnotify talks to systemd: it says when a program is ready, and keeps
// telling it that the program still works, so that systemd restarts a program that
// froze (the watchdog, WatchdogSec in the service file). It uses the standard library
// only: the protocol is one datagram on the Unix socket named by $NOTIFY_SOCKET.
//
// Outside systemd (a developer's machine, the tests of other packages) there is no
// socket and every call does nothing.
package sdnotify

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strconv"
	"time"
)

// Notifier sends messages to systemd. Its zero value, and a nil one, do nothing.
type Notifier struct {
	socket   string
	interval time.Duration // the watchdog's period (WatchdogSec), 0 when off
}

// FromEnv reads what systemd gave the program: the socket and the watchdog period.
// It returns a notifier that does nothing when the program was not started by
// systemd with Type=notify.
func FromEnv() *Notifier {
	n := &Notifier{socket: os.Getenv("NOTIFY_SOCKET")}
	if n.socket == "" {
		return n
	}
	// WATCHDOG_PID, when set, names the process the watchdog is for: not a child.
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return n
	}
	if us, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64); err == nil && us > 0 {
		n.interval = time.Duration(us) * time.Microsecond
	}
	return n
}

// Interval is the watchdog's period: systemd restarts the program if it hears
// nothing for that long. Zero when the watchdog is off.
func (n *Notifier) Interval() time.Duration {
	if n == nil {
		return 0
	}
	return n.interval
}

// Ready says the program has started (Type=notify).
func (n *Notifier) Ready() error { return n.send("READY=1") }

// Stopping says the program is stopping on purpose.
func (n *Notifier) Stopping() error { return n.send("STOPPING=1") }

// Watchdog says the program still works.
func (n *Notifier) Watchdog() error { return n.send("WATCHDOG=1") }

func (n *Notifier) send(state string) error {
	if n == nil || n.socket == "" {
		return nil
	}
	// A name starting with @ is an abstract socket (Linux); Go reads it so.
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: n.socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

// Keepalive tells systemd the program still works, until ctx is cancelled, but only
// while alive says so: alive checks that the program's loops still answer. A
// program whose loop is stuck stops sending, and systemd restarts it after the
// watchdog's period. It returns at once when the watchdog is off.
//
// It checks three times per period, each check given half a period at most: one
// slow check (the SD card busy) does not get the program killed, a stuck loop does.
func (n *Notifier) Keepalive(ctx context.Context, alive func(context.Context) error, log *slog.Logger) {
	period := n.Interval()
	if period <= 0 {
		return
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	every := period / 3
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	failing, unreachable := false, false
	for {
		checkCtx, cancel := context.WithTimeout(ctx, period/2)
		err := alive(checkCtx)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			if !failing {
				log.Error("the program does not answer: systemd will restart it if this lasts", "err", err, "after", period)
			}
			failing = true
		default:
			if failing {
				log.Warn("the program answers again")
			}
			failing = false
			err := n.Watchdog()
			if err != nil && !unreachable {
				log.Warn("could not reach systemd", "err", err)
			}
			unreachable = err != nil
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
