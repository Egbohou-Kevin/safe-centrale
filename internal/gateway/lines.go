package gateway

import (
	"context"
	"errors"
	"time"

	"safehome/internal/gsm"
)

// Modem is what the gateway needs from one modem. Errors may wrap one of the Err
// values below to say why; any other error counts as a modem error.
type Modem interface {
	// SendSMS sends a text and returns the operator's reference, used to match the
	// delivery report (empty if none).
	SendSMS(ctx context.Context, number, text string) (ref string, err error)
	// Call rings a number for ringFor, then hangs up.
	Call(ctx context.Context, number string, ringFor time.Duration) error
	Check(ctx context.Context) (LineInfo, error)
	Restart(ctx context.Context) error
}

// LineInfo is what a modem says about its network.
type LineInfo struct {
	Registered bool
	Signal     int // 0 to 100
	Operator   string
}

// Why a modem failed.
var (
	ErrNoNetwork   = errors.New("gateway: no network")
	ErrNoCredit    = errors.New("gateway: no credit")
	ErrBusy        = errors.New("gateway: busy")
	ErrUnreachable = errors.New("gateway: unreachable")
	ErrTimeout     = errors.New("gateway: timeout")
)

// worker runs a line's tasks one at a time: an AT modem takes one command at a
// time.
func worker(ctx context.Context, tasks <-chan func(context.Context)) {
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-tasks:
			t(ctx)
		}
	}
}

// toLine hands a task to a line's worker, without waiting.
func (g *Gateway) toLine(line string, task func(context.Context)) {
	select {
	case g.workers[line] <- task:
	default:
		// The domain gives a line one thing at a time; a full queue means a modem stuck
		// far longer than its deadlines. The domain's timers will recover.
		g.log.Error("the line's work queue is full", "line", line)
	}
}

// execute sends an SMS or rings a number, and hands the outcome to the domain.
func (g *Gateway) execute(ctx context.Context, ex gsm.Execute) {
	timeout := g.cfg.GSM.SMSTimeout
	if ex.Channel == gsm.ChannelCall {
		timeout += ex.RingFor
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var ref string
	var err error
	if ex.Channel == gsm.ChannelCall {
		err = g.deps.Lines[ex.Line].Call(cctx, ex.Number, ex.RingFor)
	} else {
		ref, err = g.deps.Lines[ex.Line].SendSMS(cctx, ex.Number, ex.Text)
	}
	if ctx.Err() != nil {
		return // shutting down: the job stays queued on disk
	}
	o := gsm.Outcome{Job: ex.Job, Line: ex.Line, OK: err == nil, MsgRef: ref}
	if err != nil {
		o.Reason = reason(err)
		g.log.Warn("a line could not send", "line", ex.Line, "channel", ex.Channel, "reason", o.Reason, "err", err)
	}
	g.submit(func(now time.Time) gsm.Input { o.Now = now; return o })
}

// reason maps a modem error to the domain's reason.
func reason(err error) gsm.Reason {
	switch {
	case errors.Is(err, ErrNoNetwork):
		return gsm.ReasonNoNetwork
	case errors.Is(err, ErrNoCredit):
		return gsm.ReasonNoCredit
	case errors.Is(err, ErrBusy):
		return gsm.ReasonBusy
	case errors.Is(err, ErrUnreachable):
		return gsm.ReasonUnreachable
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return gsm.ReasonTimeout
	}
	return gsm.ReasonModemError
}

// check asks the modem how the line looks, and hands the answer to the domain.
func (g *Gateway) check(ctx context.Context, line string) {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.GSM.CheckTimeout)
	defer cancel()
	info, err := g.deps.Lines[line].Check(cctx)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		g.log.Warn("a line check failed", "line", line, "err", err)
	}
	g.submit(func(now time.Time) gsm.Input {
		return gsm.Probe{Now: now, Line: line, Responding: err == nil, Registered: info.Registered, Signal: info.Signal, Operator: info.Operator}
	})
}

// restart restarts the modem. The domain is told it is over whatever the result:
// the next checks say whether it worked.
func (g *Gateway) restart(ctx context.Context, line string) {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.GSM.RestartTimeout)
	defer cancel()
	err := g.deps.Lines[line].Restart(cctx)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		g.log.Error("restarting the modem failed", "line", line, "err", err)
	}
	g.submit(func(now time.Time) gsm.Input { return gsm.Restarted{Now: now, Line: line} })
}
