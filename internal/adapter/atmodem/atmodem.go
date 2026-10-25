// Package atmodem drives a GSM modem through AT commands (3GPP TS 27.005 and 27.007)
// over a serial port: send an SMS in text mode with a delivery report, ring a number,
// read incoming SMS (in PDU mode, see decodeDeliver), report the network state,
// restart the modem.
//
// It needs nothing beyond the standard library: the port is any io.ReadWriteCloser
// (the serial device file, configured beforehand with stty). Modems differ in their
// details; what is here follows the standard and must be checked on the real modem
// (see docs/gsm-gateway.md).
//
// One command runs at a time. A goroutine reads the port, hands the answer lines to
// the command in progress and handles the unsolicited ones (new SMS, delivery
// report, end of a call).
package atmodem

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Why something failed.
var (
	ErrNoNetwork   = errors.New("atmodem: no network")
	ErrNoCredit    = errors.New("atmodem: refused by the operator (no credit?)")
	ErrBusy        = errors.New("atmodem: busy")
	ErrUnreachable = errors.New("atmodem: unreachable")
	ErrTimeout     = errors.New("atmodem: timeout")
	ErrClosed      = errors.New("atmodem: port closed")
	ErrModem       = errors.New("atmodem: modem error")
)

// Options configures a modem.
type Options struct {
	// Open opens the port. It is called again after a restart: a USB modem comes
	// back as a new device.
	Open func() (io.ReadWriteCloser, error)
	// OnSMS receives each incoming SMS. It is called from the modem's own goroutine
	// and must return quickly.
	OnSMS func(from, text string)
	// OnDeliveryReport receives the operator's report for the SMS with reference ref.
	OnDeliveryReport func(ref string, ok bool)

	CommandTimeout time.Duration // per command, 10 s by default
	// SMSTimeout bounds sending one SMS (AT+CMGS), 120 s by default: the network
	// decides how long it takes, up to the 120 s the modems' manuals give. Cut off
	// earlier, an SMS that left anyway would be reported failed and sent again.
	SMSTimeout  time.Duration
	RestartWait time.Duration // after a restart, before reopening: 20 s by default
	Logger      *slog.Logger  // nil discards
}

// Info is what the modem says about the network.
type Info struct {
	Registered bool
	Signal     int // 0 to 100
	Operator   string
}

// Modem is one modem.
type Modem struct {
	o   Options
	log *slog.Logger

	cmdMu    sync.Mutex // one command at a time
	reviveMu sync.Mutex // one reopening at a time

	mu      sync.Mutex
	port    io.ReadWriteCloser
	pending chan string // the answer lines of the command in progress
	dialing bool        // the command in progress is ATD: call results are its own
	calls   chan string // call progress after ATD (BUSY, NO CARRIER...)
	// pduMode: a reading of stored SMS switched the modem to PDU mode and may not have
	// put text mode back; the next SMS sent does (see textMode).
	pduMode bool
	// unsynced: a command timed out, and its late answer may still come; the next
	// command waits for it to be over first (see resync).
	unsynced bool
	closed   bool // the port is closed: commands fail until connect
	// gone: the port ended by itself (a USB modem unplugged, or reset on its own),
	// not closed by us. The next request opens it again.
	gone bool

	inbox  chan int     // indexes of SMS to read
	queued map[int]bool // the indexes in inbox, or being read: each is read once
	done   chan struct{}
}

// setup prepares the modem after each opening of the port.
var setup = []string{
	"ATE0",               // no echo
	"AT+CMEE=1",          // numeric error codes
	"AT+CMGF=1",          // SMS in text mode
	"AT+CSMP=49,167,0,0", // ask for a delivery report
	"AT+CNMI=2,1,0,1,0",  // tell us of new SMS (+CMTI) and reports (+CDS)
}

// Open opens the port and prepares the modem.
func Open(ctx context.Context, o Options) (*Modem, error) {
	if o.Open == nil {
		return nil, errors.New("atmodem: Open is required")
	}
	if o.CommandTimeout <= 0 {
		o.CommandTimeout = 10 * time.Second
	}
	if o.SMSTimeout <= 0 {
		o.SMSTimeout = 120 * time.Second
	}
	if o.RestartWait <= 0 {
		o.RestartWait = 20 * time.Second
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Modem{o: o, log: log, calls: make(chan string, 4), inbox: make(chan int, 32), queued: map[int]bool{}, done: make(chan struct{})}
	if err := m.connect(ctx); err != nil {
		return nil, err
	}
	go m.readInbox()
	return m, nil
}

// connect opens the port, starts reading it and sets the modem up. A modem that
// does not take its setup is closed again: half set up, it would look fine to a
// check and fail every SMS. Then the SMS stored meanwhile are queued to be read.
func (m *Modem) connect(ctx context.Context) error {
	port, err := m.o.Open()
	if err != nil {
		return fmt.Errorf("atmodem: opening the port: %w", err)
	}
	m.mu.Lock()
	if m.isDone() {
		m.mu.Unlock()
		port.Close()
		return ErrClosed
	}
	m.port, m.closed, m.gone, m.unsynced = port, false, false, false
	m.pduMode = true // until the setup sets text mode
	m.mu.Unlock()
	go m.read(port)
	for _, c := range setup {
		if _, err := m.command(ctx, c); err != nil {
			m.dropPort()
			return fmt.Errorf("atmodem: setting up the modem (%s): %w", c, err)
		}
	}
	m.mu.Lock()
	m.pduMode = false
	m.mu.Unlock()
	if err := m.sweep(ctx); err != nil {
		m.log.Warn("listing the SMS stored on the modem failed", "err", err)
	}
	return nil
}

// revive opens the port again if it went away by itself. Without it, a modem
// unplugged and plugged back would stay lost until the gateway decides to restart
// it, which it does only on a line idle long enough: never during an alarm, when
// every SMS fails at once and the line stays busy.
func (m *Modem) revive(ctx context.Context) error {
	m.reviveMu.Lock()
	defer m.reviveMu.Unlock()
	m.mu.Lock()
	gone := m.gone && !m.isDone()
	m.mu.Unlock()
	if !gone {
		return nil
	}
	m.log.Warn("the modem's port went away, opening it again")
	m.dropPort() // releases the dead one
	return m.reconnect(ctx)
}

// reconnect connects again, and leaves the port to be opened at the next request if
// that fails: a USB modem takes some seconds to come back, and a single try made
// before it did would leave every later request failing until the gateway restarts
// the modem, minutes later during an alarm.
func (m *Modem) reconnect(ctx context.Context) error {
	err := m.connect(ctx)
	if err != nil {
		m.mu.Lock()
		m.gone = !m.isDone()
		m.mu.Unlock()
	}
	return err
}

// dropPort closes the port; commands fail with ErrClosed until the next connect.
func (m *Modem) dropPort() {
	m.mu.Lock()
	port := m.port
	m.closed, m.gone = true, false
	m.mu.Unlock()
	if port != nil {
		port.Close()
	}
}

// isDone reports whether Close was called.
func (m *Modem) isDone() bool {
	select {
	case <-m.done:
		return true
	default:
		return false
	}
}

// Close stops the modem.
func (m *Modem) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isDone() {
		close(m.done)
	}
	m.closed = true
	if m.port == nil {
		return nil
	}
	return m.port.Close()
}
