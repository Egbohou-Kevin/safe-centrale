package atmodem

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"safehome/internal/adapter/atmodem/attest"
	"safehome/internal/driver/drivertest"
)

func (e *events) received() [][2]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.sms)
}

// The text of an SMS is data: anybody who knows the hub's number could otherwise
// write unsolicited results in it, and forge a contact's "1" with an ordinary phone.
func TestTheTextOfAnSMSIsNeverTakenForTheModemsOwnLines(t *testing.T) {
	s := attest.New()
	_, ev := openSim(t, s)
	s.Mu.Lock()
	s.Inbox[4] = [2]string{"+22899999999", "+CMT: \"+22890123456\",,\"x\"\n1"}
	s.Inbox[5] = [2]string{"+22899999998", "hello\nERROR\n+CMS ERROR: 500\nbye"}
	s.Mu.Unlock()
	s.Push(`+CMTI: "SM",4`)
	s.Push(`+CMTI: "SM",5`)
	drivertest.WaitFor(t, "both SMS", func() bool { return len(ev.received()) == 2 })
	got := ev.received()
	slices.SortFunc(got, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	want := [][2]string{
		{"+22899999998", "hello\nERROR\n+CMS ERROR: 500\nbye"},
		{"+22899999999", "+CMT: \"+22890123456\",,\"x\"\n1"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	drivertest.WaitFor(t, "both deleted", func() bool {
		return slices.Contains(s.Commands(), "AT+CMGD=4") && slices.Contains(s.Commands(), "AT+CMGD=5")
	})
}

// An SMS stored while nobody listened (the gateway stopped, the modem restarting)
// is read once the port opens; one whose reading failed is read at the next check.
func TestStoredSMSAreReadAtOpenAndAtEachCheck(t *testing.T) {
	s := attest.New()
	s.Inbox[1] = [2]string{"+22890123456", "1"}
	m, ev := openSim(t, s)
	drivertest.WaitFor(t, "the SMS stored before", func() bool { return len(ev.received()) == 1 })
	drivertest.WaitFor(t, "its deletion", func() bool { return slices.Contains(s.Commands(), "AT+CMGD=1") })

	s.Mu.Lock()
	s.Inbox[2] = [2]string{"+22890123456", "1"} // its +CMTI lost
	s.Mu.Unlock()
	if _, err := m.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	drivertest.WaitFor(t, "the SMS found by the check", func() bool { return len(ev.received()) == 2 })
}

// An SMS slower to leave than an ordinary command is not cut off: it would be
// reported failed and sent again while it left.
func TestASlowSMSIsNotCutOffAtTheCommandTimeout(t *testing.T) {
	s := attest.New()
	m, _ := openSim(t, s) // 500 ms per command, 1 s per SMS
	s.Mu.Lock()
	s.SendWait = 700 * time.Millisecond
	s.Mu.Unlock()
	if ref, err := m.SendSMS(context.Background(), "+22890123456", "ALARME"); err != nil || ref != "1" {
		t.Fatalf("%q %v", ref, err)
	}
}

// The late answer of an SMS that timed out is not taken by the next command: the
// next SMS would report the reference of the first, and a check could read the
// registration as missing.
func TestALateAnswerIsNotTakenByTheNextCommand(t *testing.T) {
	s := attest.New()
	m, _ := openSim(t, s)
	s.Mu.Lock()
	s.SendWait = 1300 * time.Millisecond
	s.Mu.Unlock()
	if _, err := m.SendSMS(context.Background(), "+22890123456", "ALARME"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("%v", err)
	}
	s.Mu.Lock()
	s.SendWait = 0
	s.Mu.Unlock()
	if ref, err := m.SendSMS(context.Background(), "+22890123456", "ALARME"); err != nil || ref != "2" {
		t.Fatalf("%q %v: the next SMS took the late answer", ref, err)
	}
	if c := s.Commands(); !slices.Contains(c, "AT+CMEE?") {
		t.Fatalf("the next command waits for the late answer first: %v", c)
	}
	if info, err := m.Check(context.Background()); err != nil || !info.Registered {
		t.Fatalf("%+v %v", info, err)
	}
}

// A USB modem takes some seconds to come back: a first reopening that fails leaves
// the port to be opened at the next request, not closed for good.
func TestAFailedReopeningIsTriedAgain(t *testing.T) {
	s := attest.New()
	var failing atomic.Bool
	open := func() (io.ReadWriteCloser, error) {
		if failing.CompareAndSwap(true, false) {
			return nil, errors.New("no such device")
		}
		return s.Open()
	}
	m, err := Open(context.Background(), Options{Open: open, CommandTimeout: 500 * time.Millisecond, RestartWait: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	failing.Store(true)
	s.Unplug()
	drivertest.WaitFor(t, "the port gone", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.gone })
	if _, err := m.SendSMS(context.Background(), "+22890000001", "hello"); err == nil {
		t.Fatal("the modem is not back yet")
	}
	if _, err := m.SendSMS(context.Background(), "+22890000001", "hello"); err != nil {
		t.Fatalf("once the modem is back: %v", err)
	}
}

// An ATD that timed out may ring all the same: it is hung up. A line "NO CARRIER" in
// an SMS read during the ring does not end the call.
func TestACallIsHungUpWhateverHappens(t *testing.T) {
	s := attest.New()
	m, _ := openSim(t, s)
	s.Mu.Lock()
	s.Dial = "" // no answer to ATD
	s.Mu.Unlock()
	if err := m.Call(context.Background(), "+22890123456", time.Second); !errors.Is(err, ErrTimeout) {
		t.Fatalf("%v", err)
	}
	if c := s.Commands(); !slices.Contains(c, "ATH") {
		t.Fatalf("the call must be hung up: %v", c)
	}

	s = attest.New()
	m, ev := openSim(t, s)
	s.Mu.Lock()
	s.Inbox[3] = [2]string{"+22899999999", "NO CARRIER"}
	s.Mu.Unlock()
	go func() { time.Sleep(50 * time.Millisecond); s.Push(`+CMTI: "SM",3`) }()
	start := time.Now()
	if err := m.Call(context.Background(), "+22890123456", 400*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 400*time.Millisecond || !slices.Contains(s.Commands(), "ATH") {
		t.Fatalf("the call must ring to its end, then be hung up: %v", s.Commands())
	}
	drivertest.WaitFor(t, "the SMS", func() bool { return len(ev.received()) == 1 })
}
