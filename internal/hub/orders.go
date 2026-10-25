package hub

import (
	"context"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
	engwire "safehome/internal/engine/wire"
	"safehome/internal/inbox"
)

var keypadCommands = map[string]string{
	contract.ValArm:      contract.CmdArm,
	contract.ValArmNow:   contract.CmdArmNow,
	contract.ValDisarm:   contract.CmdDisarm,
	contract.ValSilence:  contract.CmdSilence,
	contract.ValPostpone: contract.CmdPostpone,
}

// orderMaxAge is how long an order without an expiry of its own stays good after it
// was written. The broker keeps the hub's session for a day: a key pressed while the
// hub was down must not run when it comes back, hours later.
const orderMaxAge = time.Minute

// order is an order on its way to the engine.
type order struct {
	ref     string // id of the message, echoed in the acknowledgement
	cmd     contract.AlarmCommandData
	written time.Time // the message's ts: the order's age counts when cmd has no Exp
	check   bool      // its code is to be checked first, by the auth worker
}

// age is how long ago the order was written, on the hub's clock, which also stamps
// the messages of the site; a time ahead of it counts as now.
func (h *Hub) age(o order) time.Duration {
	return max(h.deps.Clock.Now().Sub(o.written), 0)
}

// expired tells whether the order is past its expiry, its own or the default one.
func (h *Hub) expired(o order) bool {
	if o.cmd.Exp != nil {
		return h.deps.Clock.Now().After(o.cmd.Exp.Time)
	}
	return h.age(o) > orderMaxAge
}

// engineCommand builds the engine's input for the order at the engine loop's time
// now. The order's expiry, its own (an order from the app) or the default one, is
// carried over to the loop's time line as the time it has left on the hub's clock,
// which checked it (expired): the two need not agree, and a fresh order must never be
// answered expired by the engine after the hub took it.
func (h *Hub) engineCommand(o order, now time.Time, auth engine.Auth) engine.Command {
	c := engwire.Command(o.cmd, o.ref, now, auth)
	left := orderMaxAge - h.age(o)
	if o.cmd.Exp != nil {
		left = o.cmd.Exp.Sub(h.deps.Clock.Now())
	}
	c.Exp = now.Add(left)
	return c
}

// keypad turns a key sequence from a keypad into an order to the engine.
func (h *Hub) keypad(env contract.Envelope, d contract.DeviceEventData) {
	name, ok := keypadCommands[d.StringValue()]
	if !ok {
		return
	}
	h.command(order{ref: env.ID, written: env.TS.Time, cmd: contract.AlarmCommandData{
		Command: name, Origin: contract.OriginKeypad,
		Args: contract.CommandArgs{Code: d.Code},
	}})
}

func (h *Hub) alarmMessage(m inbox.Message) {
	env, d, ok := decode[contract.AlarmCommandData](h, m, contract.TypeAlarmCommand, "order")
	if !ok {
		return
	}
	h.command(order{ref: env.ID, written: env.TS.Time, cmd: d})
}

// command routes an order to the engine. An order that needs a code has it checked
// first, on a worker of its own: the check is deliberately slow and must not hold up
// the siren's schedule or the broker's goroutine. The engine is told at once that a
// code is being checked (engine.CodeEntered): the end of an entry delay waits for it,
// so that a right code typed in time does not set the alarm off because its check
// ended late. While orders wait for the worker, the others queue behind them: the
// engine gets the orders in the order they came, and an "arm" pressed right after a
// "disarm" with its code is not carried out first. An expired order goes straight to
// the engine, which answers so: its code is not checked, nor counted.
func (h *Hub) command(o order) {
	needs, expired := contract.NeedsCode(o.cmd.Command), h.expired(o)
	if expired || !needs {
		o.cmd.Args.Code = ""
	}
	o.check = needs && o.cmd.Args.Code != ""
	if expired || (!o.check && h.checking.Load() == 0) {
		h.submitCommand(o, engine.Auth{}) // no code checked: the engine counts a wrong one if it needed one
		return
	}
	if o.check {
		err := h.alarm.loop.Submit(h.life, func(now time.Time) engine.Input { return engine.CodeEntered{Now: now, Ref: o.ref} })
		h.submitFailed(err)
	}
	h.checking.Add(1)
	select {
	case h.auth <- o:
	default:
		h.checking.Add(-1)
		if !o.check {
			h.submitCommand(o, engine.Auth{})
			return
		}
		// A flood of code checks: refuse without spending the CPU, and count it as
		// what it is, a wrong code.
		h.log.Warn("too many code checks waiting, refusing this order")
		o.cmd.Args.Code = ""
		h.submitCommand(o, engine.Auth{Checked: true})
	}
}

func (h *Hub) authWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case o := <-h.auth:
			h.checkOrder(o)
			h.checking.Add(-1) // once the order is queued for the engine: what follows keeps its place
		}
	}
}

// checkOrder checks the code of an order and hands it to the engine with the result;
// an order queued only to keep its place goes as it is.
func (h *Hub) checkOrder(o order) {
	if !o.check {
		h.submitCommand(o, engine.Auth{})
		return
	}
	user, ok := h.codes.Verify(o.cmd.Args.Code)
	if !ok && h.installerCode(o.cmd) {
		h.openInstall(o)
		return
	}
	o.cmd.Args.Code = "" // the code has done its work
	h.submitCommand(o, engine.Auth{Checked: true, OK: ok, User: user})
}

func (h *Hub) submitCommand(o order, auth engine.Auth) {
	err := h.alarm.loop.Submit(h.life, func(now time.Time) engine.Input {
		return h.engineCommand(o, now, auth)
	})
	h.submitFailed(err)
}
