package main

import (
	"context"
	"errors"
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
	"safehome/internal/gateway"
	"safehome/internal/gsmconfig"
)

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

// openModems prepares a modem for each line, opened on first use. The modems call
// the gateway back (incoming SMS, delivery reports), and the gateway needs the
// modems: gw is set once the gateway exists. closeAll closes those opened.
func openModems(cfg gsmconfig.Result, gw *atomic.Pointer[gateway.Gateway], log *slog.Logger) (lines map[string]gateway.Modem, closeAll func()) {
	lines = map[string]gateway.Modem{}
	var modems []*lazyModem
	for _, l := range cfg.Gateway.GSM.Lines {
		port := cfg.Ports[l.ID]
		m := &lazyModem{opts: atmodem.Options{
			Open:   func() (io.ReadWriteCloser, error) { return openPort(port) },
			Logger: log.With("line", l.ID),
			OnSMS: func(from, text string, sent time.Time) {
				if g := gw.Load(); g != nil {
					g.Incoming(l.ID, from, text, sent)
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
	return lines, func() {
		for _, m := range modems {
			m.close()
		}
	}
}
