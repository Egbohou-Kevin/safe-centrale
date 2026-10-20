package atmodem

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/adapter/atmodem/attest"
)

type events struct {
	mu      sync.Mutex
	sms     [][2]string
	reports []string
}

func (e *events) onSMS(from, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sms = append(e.sms, [2]string{from, text})
}

func (e *events) onReport(ref string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ok {
		e.reports = append(e.reports, ref+":ok")
	} else {
		e.reports = append(e.reports, ref+":failed")
	}
}

func (e *events) counts() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.sms), len(e.reports)
}

func openSim(t *testing.T, s *attest.Sim) (*Modem, *events) {
	t.Helper()
	ev := &events{}
	m, err := Open(context.Background(), Options{
		Open: s.Open, OnSMS: ev.onSMS, OnDeliveryReport: ev.onReport,
		CommandTimeout: 500 * time.Millisecond, RestartWait: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, ev
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOpenSetsTheModemUp(t *testing.T) {
	s := attest.New()
	openSim(t, s)
	want := []string{"ATE0", "AT+CMEE=1", "AT+CMGF=1", "AT+CSMP=49,167,0,0", "AT+CNMI=2,1,0,1,0"}
	if got := s.Commands(); !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
	if _, err := Open(context.Background(), Options{}); err == nil {
		t.Fatal("Open is required")
	}
	if _, err := Open(context.Background(), Options{Open: func() (io.ReadWriteCloser, error) { return nil, errors.New("no such device") }}); err == nil {
		t.Fatal("a port that does not open")
	}
}

func TestSendSMS(t *testing.T) {
	s := attest.New()
	m, _ := openSim(t, s)
	ref, err := m.SendSMS(context.Background(), "+22890123456", "ALARME Boutique\x1a\nRepondre 1")
	if err != nil || ref != "1" {
		t.Fatalf("%q %v", ref, err)
	}
	s.Mu.Lock()
	got := s.Sent[0]
	s.Mu.Unlock()
	if got[0] != "+22890123456" || got[1] != "ALARME Boutique  Repondre 1" {
		t.Fatalf("%q: control characters must never reach the modem", got)
	}
	if _, err := m.SendSMS(context.Background(), "call me", "x"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("%v", err)
	}
}

func TestSMSErrorsAreMapped(t *testing.T) {
	for line, want := range map[string]error{
		"+CMS ERROR: 21":  ErrNoCredit,
		"+CMS ERROR: 331": ErrNoNetwork,
		"+CMS ERROR: 332": ErrTimeout,
		"+CMS ERROR: 500": ErrModem,
		"+CME ERROR: 30":  ErrNoNetwork,
		"+CME ERROR: 31":  ErrTimeout,
		"+CME ERROR: 10":  ErrModem,
		"ERROR":           ErrModem,
	} {
		s := attest.New()
		m, _ := openSim(t, s)
		s.Mu.Lock()
		s.CMGS = line
		s.Mu.Unlock()
		if _, err := m.SendSMS(context.Background(), "+22890123456", "x"); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", line, err, want)
		}
	}
}

func TestCheck(t *testing.T) {
	s := attest.New()
	m, _ := openSim(t, s)
	info, err := m.Check(context.Background())
	if err != nil || !info.Registered || info.Signal != 64 || info.Operator != "TOGOCOM" {
		t.Fatalf("%+v %v", info, err)
	}
	s.Mu.Lock()
	s.CREG, s.CSQ = "0,5", "99,99" // roaming, signal unknown
	s.Mu.Unlock()
	if info, _ := m.Check(context.Background()); !info.Registered || info.Signal != 0 {
		t.Fatalf("%+v", info)
	}
	s.Mu.Lock()
	s.CREG, s.CSQ = "0,2", "31,0" // searching
	s.Mu.Unlock()
	if info, _ := m.Check(context.Background()); info.Registered || info.Signal != 100 {
		t.Fatalf("%+v", info)
	}
}

func TestASilentModemTimesOut(t *testing.T) {
	s := attest.New()
	m, _ := openSim(t, s)
	s.Mu.Lock()
	s.Silent = true
	s.Mu.Unlock()
	if _, err := m.Check(context.Background()); !errors.Is(err, ErrTimeout) {
		t.Fatalf("%v", err)
	}
	// A prompt that never comes is left cleanly too.
	if _, err := m.SendSMS(context.Background(), "+22890123456", "x"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("%v", err)
	}
}

func TestCalls(t *testing.T) {
	cases := []struct {
		name        string
		dial, after string
		wait        time.Duration
		want        error
	}{
		{"rings to the end", "OK", "", 0, nil},
		{"not answered", "OK", "NO ANSWER", 20 * time.Millisecond, nil},
		{"busy", "OK", "BUSY", 20 * time.Millisecond, ErrBusy},
		{"ends at once", "OK", "NO CARRIER", 0, ErrUnreachable},
		{"no network", "NO DIALTONE", "", 0, ErrNoNetwork},
		{"refused at once", "BUSY", "", 0, ErrBusy},
	}
	for _, c := range cases {
		s := attest.New()
		m, _ := openSim(t, s)
		s.Mu.Lock()
		s.Dial, s.AfterDial, s.AfterWait = c.dial, c.after, c.wait
		s.Mu.Unlock()
		err := m.Call(context.Background(), "+22890123456", 150*time.Millisecond)
		if !errors.Is(err, c.want) && !(err == nil && c.want == nil) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
		if c.name == "rings to the end" && !slices.Contains(s.Commands(), "ATH") {
			t.Errorf("the call must be hung up: %v", s.Commands())
		}
		if c.name == "rings to the end" && !slices.Contains(s.Commands(), "ATD+22890123456;") {
			t.Errorf("voice call: %v", s.Commands())
		}
	}
	s := attest.New()
	m, _ := openSim(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := m.Call(ctx, "+22890123456", time.Hour); !errors.Is(err, ErrTimeout) {
		t.Fatalf("%v", err)
	}
	if err := m.Call(context.Background(), "x", time.Second); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("%v", err)
	}
}

func TestIncomingSMSAreReadThenDeleted(t *testing.T) {
	s := attest.New()
	_, ev := openSim(t, s)
	s.Mu.Lock()
	s.Inbox[3] = [2]string{"+22890123456", "1"}
	s.Mu.Unlock()
	s.Push(`+CMTI: "SM",3`)
	waitFor(t, "the SMS", func() bool { n, _ := ev.counts(); return n == 1 })
	ev.mu.Lock()
	got := ev.sms[0]
	ev.mu.Unlock()
	if got != [2]string{"+22890123456", "1"} {
		t.Fatalf("%q", got)
	}
	waitFor(t, "the deletion", func() bool { return slices.Contains(s.Commands(), "AT+CMGD=3") })
	// An SMS delivered directly, without being stored.
	s.Push(`+CMT: "+22870000001",,"26/10/17,21:06:00+00"`)
	s.Push("STOP")
	waitFor(t, "the direct SMS", func() bool { n, _ := ev.counts(); return n == 2 })
}

func TestDeliveryReports(t *testing.T) {
	s := attest.New()
	_, ev := openSim(t, s)
	s.Push(`+CDS: 6,7,"+22890123456",145,"26/10/17,21:05:00+00","26/10/17,21:05:03+00",0`)
	s.Push(`+CDS: 6,8,"+22890123456",145,"26/10/17,21:05:00+00","26/10/17,21:05:03+00",48`) // still trying
	s.Push(`+CDS: 6,9,"+22890123456",145,"26/10/17,21:05:00+00","26/10/17,21:05:03+00",70`)
	s.Push(`+CDS: 6`) // damaged
	waitFor(t, "two reports", func() bool { _, n := ev.counts(); return n == 2 })
	ev.mu.Lock()
	got := slices.Clone(ev.reports)
	ev.mu.Unlock()
	slices.Sort(got)
	if !slices.Equal(got, []string{"7:ok", "9:failed"}) {
		t.Fatalf("%v", got)
	}
}

func TestRestartOpensThePortAgain(t *testing.T) {
	s := attest.New()
	m, _ := openSim(t, s)
	if err := m.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Mu.Lock()
	opens := s.Opens
	s.Mu.Unlock()
	if opens != 2 {
		t.Fatalf("%d opens", opens)
	}
	if _, err := m.Check(context.Background()); err != nil {
		t.Fatalf("after the restart: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Restart(ctx); !errors.Is(err, ErrTimeout) {
		t.Fatalf("%v", err)
	}
}

func TestAClosedModemRefusesCommands(t *testing.T) {
	s := attest.New()
	m, _ := openSim(t, s)
	m.Close()
	m.Close() // twice is harmless
	if _, err := m.Check(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("%v", err)
	}
}

func TestHelpers(t *testing.T) {
	if got := splitFields(`"REC UNREAD","+228,90",,"26/10/17,21:05"`); len(got) != 4 || got[1] != `"+228,90"` || got[2] != "" {
		t.Fatalf("%q", got)
	}
	if field(`+COPS: 0,0,"TOGO, COM",7`, 2) != `"TOGO, COM"` || field("+CSQ: 20", 5) != "" {
		t.Fatal("field")
	}
	for _, n := range []string{"+22890123456", "90123456"} {
		if !validNumber(n) {
			t.Errorf("%s", n)
		}
	}
	for _, n := range []string{"", "+", "12", "+228 90", "1+2345", strings.Repeat("1", 17)} {
		if validNumber(n) {
			t.Errorf("%q", n)
		}
	}
}

func TestAModemThatRefusesItsSetupIsClosedAgain(t *testing.T) {
	s := attest.New()
	s.Silent = true // the port opens, the modem does not answer
	_, err := Open(context.Background(), Options{Open: s.Open, CommandTimeout: 50 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("%v", err)
	}
	waitFor(t, "the port to be closed", func() bool { s.Mu.Lock(); defer s.Mu.Unlock(); return s.Hangups == 1 })
}

func TestARestartAfterCloseDoesNotReopenThePort(t *testing.T) {
	s := attest.New()
	m, _ := openSim(t, s)
	m.Close()
	if err := m.Restart(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("%v", err)
	}
	waitFor(t, "every port closed", func() bool { s.Mu.Lock(); defer s.Mu.Unlock(); return s.Hangups == s.Opens })
}

func TestALateOKBeforeThePromptIsNotTakenForTheSMS(t *testing.T) {
	s := attest.New()
	m, _ := openSim(t, s)
	s.Mu.Lock()
	s.BeforePrompt = "OK"
	s.Mu.Unlock()
	ref, err := m.SendSMS(context.Background(), "+22890123456", "ALARME")
	s.Mu.Lock()
	sent := len(s.Sent)
	s.Mu.Unlock()
	if err != nil || ref != "1" || sent != 1 {
		t.Fatalf("ref %q, err %v, %d sent: the SMS must really leave", ref, err, sent)
	}
}

func TestAnOverlongLineIsDropped(t *testing.T) {
	s := attest.New()
	_, ev := openSim(t, s)
	s.Push(`+CDS: 6,7,"+22890123456",145,"26/10/17,21:05:00+00","26/10/17,21:05:03+00",0` + strings.Repeat(" ", maxLine))
	s.Push(`+CDS: 6,8,"+22890123456",145,"26/10/17,21:05:00+00","26/10/17,21:05:03+00",0`)
	waitFor(t, "the next report", func() bool { ev.mu.Lock(); defer ev.mu.Unlock(); return slices.Contains(ev.reports, "8:ok") })
	time.Sleep(20 * time.Millisecond) // a report wrongly taken would be in by now
	ev.mu.Lock()
	defer ev.mu.Unlock()
	if len(ev.reports) != 1 || ev.reports[0] != "8:ok" {
		t.Fatalf("%v", ev.reports)
	}
}
