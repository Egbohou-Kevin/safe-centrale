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
	switch {
	case strings.HasPrefix(line, "+CMTI:"):
		m.announce(line)
		return
	case strings.HasPrefix(line, "+CMT:"):
		// An SMS delivered without being stored: the modem is set to store them all
		// (AT+CNMI=2,1), and the raw text that would follow such a line could hold
		// lines of its own. It is not taken.
		m.log.Warn("ignoring an SMS delivered without being stored")
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
	lines, err := m.readStored(ctx, "AT+CMGL=4") // all of them
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range lines {
		if stat := field(l, 1); !strings.HasPrefix(l, "+CMGL:") || (stat != "0" && stat != "1") {
			continue // a PDU, or an SMS we did not receive (stored to be sent)
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

// takeStored reads the SMS stored at index i, hands it over and deletes it. One that
// cannot be read is deleted all the same, or it would be read again at every check
// and fill the SIM. Each command has its own time limit, counted once it holds the
// modem: a limit for the whole would run out while an SMS is being sent.
func (m *Modem) takeStored(i int) error {
	ctx := context.Background()
	lines, err := m.readStored(ctx, fmt.Sprintf("AT+CMGR=%d", i))
	if err != nil {
		return err
	}
	for k, l := range lines {
		if !strings.HasPrefix(l, "+CMGR:") || k+1 >= len(lines) {
			continue
		}
		if from, text, err := decodeDeliver(lines[k+1]); err == nil {
			m.notifySMS(from, text)
		} else {
			m.log.Warn("an incoming SMS cannot be read, deleting it", "index", i, "err", err)
		}
		break
	}
	_, err = m.command(ctx, fmt.Sprintf("AT+CMGD=%d", i))
	return err
}
