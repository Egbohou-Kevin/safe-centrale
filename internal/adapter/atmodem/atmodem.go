// Package atmodem drives a GSM modem through AT commands (3GPP TS 27.005 and 27.007)
// over a serial port: send an SMS in text mode with a delivery report, ring a number,
// read incoming SMS, report the network state, restart the modem.
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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
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
	RestartWait    time.Duration // after a restart, before reopening: 20 s by default
	Logger         *slog.Logger  // nil discards
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

	cmdMu sync.Mutex // one command at a time

	mu      sync.Mutex
	port    io.ReadWriteCloser
	pending chan string // the answer lines of the command in progress
	dialing bool        // the command in progress is ATD: call results are its own
	calls   chan string // call progress after ATD (BUSY, NO CARRIER...)
	cmtFrom string      // an SMS delivered directly (+CMT): its text is the next line
	closed  bool

	inbox chan int // indexes of SMS to read
	done  chan struct{}
}

// Open opens the port and prepares the modem.
func Open(ctx context.Context, o Options) (*Modem, error) {
	if o.Open == nil {
		return nil, errors.New("atmodem: Open is required")
	}
	if o.CommandTimeout <= 0 {
		o.CommandTimeout = 10 * time.Second
	}
	if o.RestartWait <= 0 {
		o.RestartWait = 20 * time.Second
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Modem{o: o, log: log, calls: make(chan string, 4), inbox: make(chan int, 32), done: make(chan struct{})}
	if err := m.connect(ctx); err != nil {
		return nil, err
	}
	go m.readInbox()
	return m, nil
}

// connect opens the port, starts reading it and sets the modem up.
func (m *Modem) connect(ctx context.Context) error {
	port, err := m.o.Open()
	if err != nil {
		return fmt.Errorf("atmodem: opening the port: %w", err)
	}
	m.mu.Lock()
	m.port, m.closed = port, false
	m.mu.Unlock()
	go m.read(port)
	for _, c := range []string{
		"ATE0",               // no echo
		"AT+CMEE=1",          // numeric error codes
		"AT+CMGF=1",          // SMS in text mode
		"AT+CSMP=49,167,0,0", // ask for a delivery report
		"AT+CNMI=2,1,0,1,0",  // tell us of new SMS (+CMTI) and reports (+CDS)
	} {
		if _, err := m.command(ctx, c); err != nil {
			return fmt.Errorf("atmodem: setting up the modem (%s): %w", c, err)
		}
	}
	return nil
}

// Close stops the modem.
func (m *Modem) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-m.done:
	default:
		close(m.done)
	}
	m.closed = true
	if m.port == nil {
		return nil
	}
	return m.port.Close()
}

// --- reading the port ----------------------------------------------------------------------

func (m *Modem) read(port io.Reader) {
	r := bufio.NewReader(port)
	var line []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			m.mu.Lock()
			if m.port == port {
				m.closed = true
			}
			m.mu.Unlock()
			return
		}
		switch {
		case b == '\n':
			if s := strings.TrimRight(string(line), "\r"); s != "" {
				m.route(s)
			}
			line = line[:0]
		case b == ' ' && len(line) == 1 && line[0] == '>':
			m.route(">") // the SMS prompt has no end of line
			line = line[:0]
		default:
			line = append(line, b)
		}
	}
}

// route hands a line to whoever is waiting for it.
func (m *Modem) route(line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.cmtFrom != "":
		from := m.cmtFrom
		m.cmtFrom = ""
		m.notifySMS(from, line)
		return
	case strings.HasPrefix(line, "+CMTI:"):
		if i, err := strconv.Atoi(field(line, 1)); err == nil {
			select {
			case m.inbox <- i:
			default:
				m.log.Error("too many incoming SMS waiting to be read")
			}
		}
		return
	case strings.HasPrefix(line, "+CMT:"):
		m.cmtFrom = unquote(field(line, 0))
		return
	case strings.HasPrefix(line, "+CDS:"):
		m.deliveryReport(line)
		return
	case line == "RING":
		return // incoming calls are not answered
	case isCallResult(line):
		// The end of a call. It may also be the final answer of ATD, or arrive just
		// after ATD's OK while the command is still winding up: Call reads it from
		// here, and drains what a failed ATD left before dialing again.
		select {
		case m.calls <- line:
		default:
		}
		if !m.dialing {
			return
		}
	}
	if m.pending != nil {
		select {
		case m.pending <- line:
		default:
			m.log.Error("modem answer lost: the command reads too slowly")
		}
	}
}

func isCallResult(line string) bool {
	switch line {
	case "BUSY", "NO CARRIER", "NO ANSWER", "NO DIALTONE":
		return true
	}
	return false
}

// deliveryReport reads "+CDS: <fo>,<mr>,<ra>,<tora>,<scts>,<dt>,<st>".
func (m *Modem) deliveryReport(line string) {
	fields := splitFields(strings.TrimSpace(strings.TrimPrefix(line, "+CDS:")))
	if len(fields) < 7 || m.o.OnDeliveryReport == nil {
		return
	}
	st, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil || (st >= 32 && st < 64) {
		return // still trying: wait for the final report
	}
	ref, ok := fields[1], st < 32
	go m.o.OnDeliveryReport(ref, ok)
}

func (m *Modem) notifySMS(from, text string) {
	if m.o.OnSMS != nil {
		go m.o.OnSMS(from, text)
	}
}

// readInbox reads, then deletes, each SMS announced by +CMTI.
func (m *Modem) readInbox() {
	for {
		select {
		case <-m.done:
			return
		case i := <-m.inbox:
			ctx, cancel := context.WithTimeout(context.Background(), 2*m.o.CommandTimeout)
			lines, err := m.command(ctx, fmt.Sprintf("AT+CMGR=%d", i))
			if err == nil {
				for k, l := range lines {
					if strings.HasPrefix(l, "+CMGR:") && k+1 < len(lines) {
						m.mu.Lock()
						m.notifySMS(unquote(field(l, 1)), lines[k+1])
						m.mu.Unlock()
					}
				}
				_, err = m.command(ctx, fmt.Sprintf("AT+CMGD=%d", i))
			}
			cancel()
			if err != nil {
				m.log.Warn("reading an incoming SMS failed", "index", i, "err", err)
			}
		}
	}
}

// --- commands ------------------------------------------------------------------------------

// command sends one AT command and returns the lines of its answer, without the
// final OK. text, when given, is written after the "> " prompt and ended with Ctrl-Z.
func (m *Modem) command(ctx context.Context, cmd string, text ...string) ([]string, error) {
	m.cmdMu.Lock()
	defer m.cmdMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, m.o.CommandTimeout)
	defer cancel()

	answers := make(chan string, 64)
	m.mu.Lock()
	if m.closed || m.port == nil {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	port := m.port
	m.pending, m.dialing = answers, strings.HasPrefix(cmd, "ATD")
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.pending, m.dialing = nil, false
		m.mu.Unlock()
	}()

	if _, err := io.WriteString(port, cmd+"\r"); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrClosed, err)
	}
	var lines []string
	for {
		select {
		case <-ctx.Done():
			if len(text) > 0 {
				io.WriteString(port, "\x1b") // leave the prompt
			}
			return nil, ErrTimeout
		case l := <-answers:
			switch {
			case l == ">" && len(text) > 0:
				if _, err := io.WriteString(port, text[0]+"\x1a"); err != nil {
					return nil, fmt.Errorf("%w: %v", ErrClosed, err)
				}
			case l == "OK":
				return lines, nil
			case l == "ERROR":
				return nil, ErrModem
			case strings.HasPrefix(l, "+CMS ERROR:"), strings.HasPrefix(l, "+CME ERROR:"):
				return nil, codeError(l)
			case isCallResult(l):
				return nil, callError(l)
			case l == cmd: // an echo, before ATE0 takes effect
			default:
				lines = append(lines, l)
			}
		}
	}
}

// codeError maps the standard error codes the gateway can act upon. The codes an
// operator uses for "no credit" vary: 21 (message rejected) is the usual one and
// must be checked with Togocom and Moov.
func codeError(line string) error {
	code, _ := strconv.Atoi(strings.TrimSpace(line[strings.Index(line, ":")+1:]))
	sms := strings.HasPrefix(line, "+CMS")
	switch {
	case sms && code == 21, sms && code == 50:
		return fmt.Errorf("%w (%s)", ErrNoCredit, line)
	case sms && (code == 38 || code == 330 || code == 331), !sms && code == 30:
		return fmt.Errorf("%w (%s)", ErrNoNetwork, line)
	case sms && code == 332, !sms && code == 31:
		return fmt.Errorf("%w (%s)", ErrTimeout, line)
	}
	return fmt.Errorf("%w (%s)", ErrModem, line)
}

func callError(line string) error {
	switch line {
	case "BUSY":
		return ErrBusy
	case "NO DIALTONE":
		return ErrNoNetwork
	}
	return ErrUnreachable
}

// --- what the gateway asks -----------------------------------------------------------------

// SendSMS sends a text and returns the operator's reference.
func (m *Modem) SendSMS(ctx context.Context, number, text string) (string, error) {
	if !validNumber(number) {
		return "", fmt.Errorf("%w: invalid number", ErrUnreachable)
	}
	lines, err := m.command(ctx, fmt.Sprintf("AT+CMGS=%q", number), clean(text))
	if err != nil {
		return "", err
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "+CMGS:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "+CMGS:")), nil
		}
	}
	return "", nil
}

// Call rings a number for ringFor, then hangs up. Ringing to the end, or being
// picked up, is a success: the contact saw the hub's number.
func (m *Modem) Call(ctx context.Context, number string, ringFor time.Duration) error {
	if !validNumber(number) {
		return fmt.Errorf("%w: invalid number", ErrUnreachable)
	}
	m.drainCalls()
	if _, err := m.command(ctx, "ATD"+number+";"); err != nil {
		return err
	}
	start := time.Now()
	timer := time.NewTimer(ringFor)
	defer timer.Stop()
	hangUp := func() {
		hctx, cancel := context.WithTimeout(context.Background(), m.o.CommandTimeout)
		defer cancel()
		m.command(hctx, "ATH")
	}
	select {
	case <-timer.C:
		hangUp()
		return nil
	case r := <-m.calls:
		switch {
		case r == "BUSY":
			hangUp()
			return ErrBusy
		case r == "NO CARRIER" && time.Since(start) < 3*time.Second:
			return ErrUnreachable // ended before it could ring
		}
		return nil
	case <-ctx.Done():
		hangUp()
		return ErrTimeout
	}
}

func (m *Modem) drainCalls() {
	for {
		select {
		case <-m.calls:
		default:
			return
		}
	}
}

// Check asks whether the modem answers and how the network looks.
func (m *Modem) Check(ctx context.Context) (Info, error) {
	if _, err := m.command(ctx, "AT"); err != nil {
		return Info{}, err
	}
	var info Info
	lines, err := m.command(ctx, "AT+CREG?")
	if err != nil {
		return Info{}, err
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "+CREG:") {
			stat := field(l, 1)
			info.Registered = stat == "1" || stat == "5" // home network or roaming
		}
	}
	if lines, err := m.command(ctx, "AT+CSQ"); err == nil {
		for _, l := range lines {
			if strings.HasPrefix(l, "+CSQ:") {
				if rssi, err := strconv.Atoi(field(l, 0)); err == nil && rssi != 99 {
					info.Signal = min(rssi*100/31, 100)
				}
			}
		}
	}
	if lines, err := m.command(ctx, "AT+COPS?"); err == nil {
		for _, l := range lines {
			if strings.HasPrefix(l, "+COPS:") {
				info.Operator = unquote(field(l, 2))
			}
		}
	}
	return info, nil
}

// Restart restarts the modem and opens the port again once it is back.
func (m *Modem) Restart(ctx context.Context) error {
	m.command(ctx, "AT+CFUN=1,1") // it may not answer: that is why it is restarted
	m.mu.Lock()
	port := m.port
	m.closed = true
	m.mu.Unlock()
	if port != nil {
		port.Close()
	}
	select {
	case <-time.After(m.o.RestartWait):
	case <-ctx.Done():
		return ErrTimeout
	}
	return m.connect(ctx)
}

// --- small helpers -------------------------------------------------------------------------

// field returns the i-th comma-separated value after the colon of an answer line.
func field(line string, i int) string {
	if k := strings.Index(line, ":"); k >= 0 {
		line = line[k+1:]
	}
	fields := splitFields(strings.TrimSpace(line))
	if i >= len(fields) {
		return ""
	}
	return fields[i]
}

// splitFields splits on commas that are not inside quotes.
func splitFields(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case r == ',' && !quoted:
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(out, strings.TrimSpace(cur.String()))
}

func unquote(s string) string { return strings.Trim(s, `"`) }

func validNumber(n string) bool {
	if len(n) < 4 || len(n) > 16 {
		return false
	}
	for i, r := range n {
		if !(r >= '0' && r <= '9') && !(i == 0 && r == '+') {
			return false
		}
	}
	return true
}

// clean keeps a text safe for text mode: no control characters (Ctrl-Z would end the
// message, ESC would cancel it), plain ASCII.
func clean(text string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return ' '
		}
		return r
	}, text)
}
