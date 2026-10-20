package atmodem

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SendSMS sends a text and returns the operator's reference.
func (m *Modem) SendSMS(ctx context.Context, number, text string) (string, error) {
	if !validNumber(number) {
		return "", fmt.Errorf("%w: invalid number", ErrUnreachable)
	}
	lines, err := m.commandWithText(ctx, fmt.Sprintf("AT+CMGS=%q", number), clean(text))
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
	select {
	case <-timer.C:
		m.hangUp()
		return nil
	case r := <-m.calls:
		switch {
		case r == "BUSY":
			m.hangUp()
			return ErrBusy
		case r == "NO CARRIER" && time.Since(start) < 3*time.Second:
			return ErrUnreachable // ended before it could ring
		}
		return nil
	case <-ctx.Done():
		m.hangUp()
		return ErrTimeout
	}
}

// hangUp ends the call, even when the caller's context is over.
func (m *Modem) hangUp() {
	ctx, cancel := context.WithTimeout(context.Background(), m.o.CommandTimeout)
	defer cancel()
	m.command(ctx, "ATH")
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

// Check asks whether the modem answers and how the network looks. Only the
// registration is required; the signal and the operator are best effort.
func (m *Modem) Check(ctx context.Context) (Info, error) {
	if _, err := m.command(ctx, "AT"); err != nil {
		return Info{}, err
	}
	var info Info
	creg, err := m.answer(ctx, "AT+CREG?", "+CREG:")
	if err != nil {
		return Info{}, err
	}
	stat := field(creg, 1)
	info.Registered = stat == "1" || stat == "5" // home network or roaming
	if csq, err := m.answer(ctx, "AT+CSQ", "+CSQ:"); err == nil {
		if rssi, err := strconv.Atoi(field(csq, 0)); err == nil && rssi != 99 {
			info.Signal = min(rssi*100/31, 100)
		}
	}
	if cops, err := m.answer(ctx, "AT+COPS?", "+COPS:"); err == nil {
		info.Operator = unquote(field(cops, 2))
	}
	return info, nil
}

// answer sends a query and returns the last answer line with the given prefix, ""
// if none.
func (m *Modem) answer(ctx context.Context, cmd, prefix string) (string, error) {
	lines, err := m.command(ctx, cmd)
	var found string
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			found = l
		}
	}
	return found, err
}

// Restart restarts the modem and opens the port again once it is back.
func (m *Modem) Restart(ctx context.Context) error {
	m.command(ctx, "AT+CFUN=1,1") // it may not answer: that is why it is restarted
	m.dropPort()
	select {
	case <-time.After(m.o.RestartWait):
	case <-ctx.Done():
		return ErrTimeout
	}
	return m.connect(ctx)
}
