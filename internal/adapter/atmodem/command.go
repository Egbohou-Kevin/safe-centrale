package atmodem

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// exchange is a command in progress: what it sent, and the answer so far.
type exchange struct {
	cmd      string
	text     string // written after the "> " prompt, when withText
	withText bool
	prompted bool // the text was written
	reading  bool // the answer reached the text of a stored SMS: data up to OK
	lines    []string
}

// command sends one AT command and returns the lines of its answer, without the
// final OK.
func (m *Modem) command(ctx context.Context, cmd string) ([]string, error) {
	return m.run(ctx, &exchange{cmd: cmd})
}

// commandWithText sends a command that prompts for a text (AT+CMGS), writes the
// text ended with Ctrl-Z, and returns the lines of the answer.
func (m *Modem) commandWithText(ctx context.Context, cmd, text string) ([]string, error) {
	return m.run(ctx, &exchange{cmd: cmd, text: text, withText: true})
}

func (m *Modem) run(ctx context.Context, x *exchange) ([]string, error) {
	m.cmdMu.Lock()
	defer m.cmdMu.Unlock()
	timeout := m.o.CommandTimeout
	if x.withText {
		timeout = m.o.SMSTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	port, answers, unsynced, err := m.begin(x.cmd)
	if err != nil {
		return nil, err
	}
	defer m.end()
	if unsynced {
		if err := m.resync(ctx, port, answers); err != nil {
			return nil, err
		}
	}
	if _, err := io.WriteString(port, x.cmd+"\r"); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrClosed, err)
	}
	for {
		select {
		case <-ctx.Done():
			if x.withText {
				io.WriteString(port, "\x1b") // leave the prompt
			}
			m.mu.Lock()
			m.unsynced = true
			m.mu.Unlock()
			return nil, ErrTimeout
		case l := <-answers:
			done, err := x.take(port, l)
			if err != nil {
				return nil, err
			}
			if done {
				return x.lines, nil
			}
		}
	}
}

// begin makes the reader hand the answer lines to the command about to be sent, and
// tells whether a late answer must be waited for first.
func (m *Modem) begin(cmd string) (io.Writer, <-chan string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.port == nil {
		return nil, nil, false, ErrClosed
	}
	answers := make(chan string, 64)
	m.pending, m.dialing = answers, strings.HasPrefix(cmd, "ATD")
	m.listing, m.inText = readsStored(cmd), false
	return m.port, answers, m.unsynced, nil
}

func (m *Modem) end() {
	m.mu.Lock()
	m.pending, m.dialing, m.listing, m.inText = nil, false, false, false
	m.mu.Unlock()
}

// readsStored tells whether a command reads stored SMS, whose text comes in its answer.
func readsStored(cmd string) bool {
	return strings.HasPrefix(cmd, "AT+CMGR=") || strings.HasPrefix(cmd, "AT+CMGL=")
}

// storedHeader tells whether line heads a stored SMS in such an answer: what follows
// it is the SMS's text.
func storedHeader(line string) bool {
	return strings.HasPrefix(line, "+CMGR:") || strings.HasPrefix(line, "+CMGL:")
}

// resync waits for the late answer of a command that timed out before the next one is
// sent: it asks for an answer that only it gets (+CMEE:), and drops every line up to
// the end of it. Otherwise the late OK of an SMS would end the next command early, and
// that command's own answer the one after: a check would read the registration as
// missing, and a reading of an SMS find no text before deleting it.
func (m *Modem) resync(ctx context.Context, port io.Writer, answers <-chan string) error {
	ctx, cancel := context.WithTimeout(ctx, m.o.CommandTimeout)
	defer cancel()
	if _, err := io.WriteString(port, "AT+CMEE?\r"); err != nil {
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}
	marked := false
	for {
		select {
		case <-ctx.Done():
			return ErrTimeout
		case l := <-answers:
			switch {
			case strings.HasPrefix(l, "+CMEE:"):
				marked = true
			case marked && (l == "OK" || l == "ERROR"):
				m.mu.Lock()
				m.unsynced = false
				m.mu.Unlock()
				return nil
			}
		}
	}
}

// take handles one answer line, and reports whether the command is over.
func (x *exchange) take(port io.Writer, l string) (done bool, err error) {
	switch {
	case x.reading && l != "OK":
		x.lines = append(x.lines, l) // the text of an SMS, whatever it says
	case readsStored(x.cmd) && storedHeader(l):
		x.reading = true
		x.lines = append(x.lines, l)
	case l == ">" && x.withText && !x.prompted:
		x.prompted = true
		if _, err := io.WriteString(port, x.text+"\x1a"); err != nil {
			return true, fmt.Errorf("%w: %v", ErrClosed, err)
		}
	case l == "OK" && x.withText && !x.prompted:
		// The late answer of an earlier command that timed out: this one says OK
		// only once its text is written. Taking it would report an SMS that never
		// left as sent.
	case l == "OK":
		return true, nil
	case l == "ERROR":
		return true, ErrModem
	case strings.HasPrefix(l, "+CMS ERROR:"), strings.HasPrefix(l, "+CME ERROR:"):
		return true, codeError(l)
	case isCallResult(l):
		return true, callError(l)
	case l == x.cmd: // an echo, before ATE0 takes effect
	default:
		x.lines = append(x.lines, l)
	}
	return false, nil
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
