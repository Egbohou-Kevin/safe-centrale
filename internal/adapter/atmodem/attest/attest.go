// Package attest simulates a GSM modem speaking AT commands over an in-memory pipe,
// for tests: it answers like a real one (OK, errors, SMS prompt, call progress,
// incoming SMS, restart). Only tests may import it.
package attest

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Sim answers like a modem over an in-memory pipe. Each open gives a new
// connection, as a USB modem does after a restart.
type Sim struct {
	Mu     sync.Mutex // guards the fields below
	conn   net.Conn
	echo   bool   // echo commands, until ATE0
	Silent bool   // read and never answer
	CREG   string // answer to AT+CREG?
	CSQ    string
	COPS   string
	CMGS   string // error line for AT+CMGS, empty for success
	// BeforePrompt is a line sent before the SMS prompt, as a late answer to an
	// earlier command would be.
	BeforePrompt string
	Dial         string // final line of ATD, "OK" by default
	AfterDial    string // call progress sent after ATD's OK, empty for none
	AfterWait    time.Duration
	Inbox        map[int][2]string // index -> from, text
	commands     []string
	Sent         [][2]string // number, text
	ref          int
	Opens        int
	Hangups      int // connections closed by the other end
}

// New returns a modem registered on the network, with a good signal.
func New() *Sim {
	return &Sim{echo: true, CREG: "0,1", CSQ: "20,99", COPS: `0,0,"TOGOCOM",7`, Dial: "OK", Inbox: map[int][2]string{}}
}

// Open connects to the modem, as opening its serial port would.
func (s *Sim) Open() (io.ReadWriteCloser, error) {
	client, server := net.Pipe()
	s.Mu.Lock()
	s.conn = server
	s.Opens++
	s.echo = true
	s.Mu.Unlock()
	go s.serve(server)
	return client, nil
}

func (s *Sim) write(c net.Conn, lines ...string) {
	for _, l := range lines {
		io.WriteString(c, "\r\n"+l+"\r\n")
	}
}

// Push sends an unsolicited line, as the modem does when an SMS arrives.
func (s *Sim) Push(line string) {
	s.Mu.Lock()
	c := s.conn
	s.Mu.Unlock()
	s.write(c, line)
}

// Unplug closes the connection from the modem's side, as pulling a USB modem out
// does; the next Open plugs it back.
func (s *Sim) Unplug() {
	s.Mu.Lock()
	c := s.conn
	s.Mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// Commands lists the commands received so far.
func (s *Sim) Commands() []string {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return append([]string(nil), s.commands...)
}

func (s *Sim) serve(c net.Conn) {
	r := bufio.NewReader(c)
	for {
		cmd, err := r.ReadString('\r')
		if err != nil {
			s.Mu.Lock()
			s.Hangups++
			s.Mu.Unlock()
			return
		}
		cmd = strings.TrimSpace(cmd)
		s.Mu.Lock()
		s.commands = append(s.commands, cmd)
		silent, echo := s.Silent, s.echo
		s.Mu.Unlock()
		if silent {
			continue
		}
		if echo {
			io.WriteString(c, cmd+"\r")
		}
		s.answer(c, r, cmd)
	}
}

func (s *Sim) answer(c net.Conn, r *bufio.Reader, cmd string) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	switch {
	case cmd == "ATE0":
		s.echo = false
		s.write(c, "OK")
	case cmd == "AT+CREG?":
		s.write(c, "+CREG: "+s.CREG, "OK")
	case cmd == "AT+CSQ":
		s.write(c, "+CSQ: "+s.CSQ, "OK")
	case cmd == "AT+COPS?":
		s.write(c, "+COPS: "+s.COPS, "OK")
	case strings.HasPrefix(cmd, "AT+CMGS="):
		s.takeSMS(c, r, cmd)
	case strings.HasPrefix(cmd, "ATD"):
		s.write(c, s.Dial)
		if s.Dial == "OK" && s.AfterDial != "" {
			line, wait := s.AfterDial, s.AfterWait
			go func() { time.Sleep(wait); s.write(c, line) }()
		}
	case strings.HasPrefix(cmd, "AT+CMGR="):
		var i int
		fmt.Sscanf(cmd, "AT+CMGR=%d", &i)
		m, ok := s.Inbox[i]
		if !ok {
			s.write(c, "+CMS ERROR: 321")
			return
		}
		s.write(c, fmt.Sprintf(`+CMGR: "REC UNREAD","%s",,"26/10/17,21:05:00+00"`, m[0]), m[1], "OK")
	case strings.HasPrefix(cmd, "AT+CMGD="):
		var i int
		fmt.Sscanf(cmd, "AT+CMGD=%d", &i)
		delete(s.Inbox, i)
		s.write(c, "OK")
	case cmd == "AT+CFUN=1,1":
		s.write(c, "OK")
		go c.Close() // the modem goes away
	default:
		s.write(c, "OK") // AT, ATH, AT+CMGF=1, ...
	}
}

// takeSMS prompts for the text of an AT+CMGS and keeps it. s.Mu is held, and
// released while the text is read.
func (s *Sim) takeSMS(c net.Conn, r *bufio.Reader, cmd string) {
	number := strings.Trim(strings.TrimPrefix(cmd, "AT+CMGS="), `"`)
	if s.BeforePrompt != "" {
		s.write(c, s.BeforePrompt)
	}
	io.WriteString(c, "\r\n> ")
	s.Mu.Unlock()
	text, err := r.ReadString('\x1a')
	s.Mu.Lock()
	if err != nil {
		return
	}
	text = strings.TrimSuffix(text, "\x1a")
	if s.CMGS != "" {
		s.write(c, s.CMGS)
		return
	}
	s.ref++
	s.Sent = append(s.Sent, [2]string{number, text})
	s.write(c, fmt.Sprintf("+CMGS: %d", s.ref), "OK")
}
