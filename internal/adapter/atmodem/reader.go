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
func (m *Modem) announce(line string) {
	i, err := strconv.Atoi(field(line, 1))
	if err != nil {
		return
	}
	select {
	case m.inbox <- i:
	default:
		m.log.Error("too many incoming SMS waiting to be read")
	}
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
				m.log.Warn("reading an incoming SMS failed", "index", i, "err", err)
			}
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
		if strings.HasPrefix(l, "+CMGR:") && k+1 < len(lines) {
			m.notifySMS(unquote(field(l, 1)), lines[k+1])
		}
	}
	_, err = m.command(ctx, fmt.Sprintf("AT+CMGD=%d", i))
	return err
}
