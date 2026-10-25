package atmodem

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// maxLine bounds an answer line: a modem gone mad must not fill the memory.
const maxLine = 1024

// read reads the port line by line until it is closed.
func (m *Modem) read(port io.Reader) {
	r := bufio.NewReader(port)
	var line []byte
	overlong := false
	for {
		b, err := r.ReadByte()
		if err != nil {
			m.mu.Lock()
			if m.port == port {
				m.gone = m.gone || !m.closed // not closed by us: it went away
				m.closed = true
			}
			m.mu.Unlock()
			return
		}
		switch {
		case b == '\n':
			if s := strings.TrimRight(string(line), "\r"); s != "" && !overlong {
				m.route(s)
			}
			line, overlong = line[:0], false
		case b == ' ' && len(line) == 1 && line[0] == '>':
			m.route(">") // the SMS prompt has no end of line
			line = line[:0]
		case len(line) >= maxLine:
			overlong = true // dropped up to the end of the line
		default:
			line = append(line, b)
		}
	}
}

// route hands a line to whoever is waiting for it.
func (m *Modem) route(line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.textLine(line) {
		m.hand(line)
		return
	}
	switch {
	case m.cmtFrom != "":
		from := m.cmtFrom
		m.cmtFrom = ""
		m.notifySMS(from, line)
		return
	case strings.HasPrefix(line, "+CMTI:"):
		m.announce(line)
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
	m.hand(line)
}

// hand gives a line to the command in progress, if any. m.mu is held.
func (m *Modem) hand(line string) {
	if m.pending != nil {
		select {
		case m.pending <- line:
		default:
			m.log.Error("modem answer lost: the command reads too slowly")
		}
	}
}

// textLine tells whether line belongs to the text of a stored SMS being read
// (AT+CMGR, AT+CMGL): from the first header of the answer to its final OK, every line
// is data, never an unsolicited result. Otherwise anybody could write one in an SMS:
// a text reading `+CMT: "<a contact>",,""` then `1` forged that contact's "1", and a
// line "NO CARRIER" ended a call under way. A text line reading exactly OK still ends
// the answer early: the format has no other end (docs/gsm-gateway.md). m.mu is held.
func (m *Modem) textLine(line string) bool {
	switch {
	case !m.listing:
		return false
	case m.inText:
		m.inText = line != "OK"
		return true
	case storedHeader(line):
		m.inText = true
		return true
	}
	return false
}

func isCallResult(line string) bool {
	switch line {
	case "BUSY", "NO CARRIER", "NO ANSWER", "NO DIALTONE":
		return true
	}
	return false
}

// announce queues the SMS stored at the index a "+CMTI:" line gives, to be read.
// m.mu is held.
func (m *Modem) announce(line string) {
	if i, err := strconv.Atoi(field(line, 1)); err == nil {
		m.queue(i)
	}
}

// queue queues the SMS stored at index i to be read, once. One that does not fit is
// found again by the next sweep. m.mu is held.
func (m *Modem) queue(i int) {
	if m.queued[i] {
		return
	}
	select {
	case m.inbox <- i:
		m.queued[i] = true
	default:
		m.log.Error("too many incoming SMS waiting to be read")
	}
}

// sweep queues the received SMS still stored on the modem, to be read: one that came
// while the gateway was stopped or the modem restarting, whose announcement was lost,
// or whose reading failed. Left there, a contact's "1" would be lost without a trace,
// and the stored SMS would fill the SIM until the network delivers no more. It runs
// after each opening of the port and at each check.
func (m *Modem) sweep(ctx context.Context) error {
	lines, err := m.command(ctx, `AT+CMGL="ALL"`)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range lines {
		if !strings.HasPrefix(l, "+CMGL:") || !strings.HasPrefix(unquote(field(l, 1)), "REC") {
			continue // the text of an SMS, or one we did not receive
		}
		if i, err := strconv.Atoi(field(l, 0)); err == nil {
			m.queue(i)
		}
	}
	return nil
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
			if err := m.takeStored(i); err != nil {
				m.log.Warn("reading an incoming SMS failed, it is read again at the next check", "index", i, "err", err)
			}
			m.mu.Lock()
			delete(m.queued, i)
			m.mu.Unlock()
		}
	}
}

// takeStored reads the SMS stored at index i, hands it over and deletes it.
func (m *Modem) takeStored(i int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*m.o.CommandTimeout)
	defer cancel()
	lines, err := m.command(ctx, fmt.Sprintf("AT+CMGR=%d", i))
	if err != nil {
		return err
	}
	for k, l := range lines {
		if strings.HasPrefix(l, "+CMGR:") {
			m.notifySMS(unquote(field(l, 1)), strings.Join(lines[k+1:], "\n"))
			break
		}
	}
	_, err = m.command(ctx, fmt.Sprintf("AT+CMGD=%d", i))
	return err
}
