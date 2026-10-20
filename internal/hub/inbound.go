package hub

import (
	"context"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
	engwire "safehome/internal/engine/wire"
	"safehome/internal/health"
	healthwire "safehome/internal/health/wire"
)

// HandleMessage takes a message from the broker. It is meant to be called from the
// MQTT client's own goroutine and returns quickly: it decodes, checks, and hands
// the work to the domain loops. A message that is not ours, or is damaged, is
// dropped with a log line that never contains the payload (it may hold a code).
func (h *Hub) HandleMessage(m Message) {
	if len(m.Payload) > maxPayload {
		h.log.Warn("ignoring an oversized message", "topic", m.Topic, "bytes", len(m.Payload))
		return
	}
	site, parts, ok := contract.ParseTopic(m.Topic)
	if !ok || site != h.cfg.Site {
		return
	}
	switch {
	case len(parts) == 3 && parts[0] == "device" && parts[2] == "event":
		h.deviceMessage(parts[1], "", m)
	case len(parts) == 4 && parts[0] == "device" && parts[2] == "state":
		h.deviceMessage(parts[1], parts[3], m)
	case len(parts) == 2 && parts[0] == "alarm" && parts[1] == "cmd":
		h.alarmMessage(m)
	case len(parts) == 2 && parts[0] == "alert" && parts[1] == "result":
		h.alertResultMessage(m)
	case len(parts) == 2 && parts[0] == "alert" && parts[1] == "ack":
		h.alertAckMessage(m)
	case len(parts) == 2 && parts[0] == "gsm" && parts[1] == "status":
		h.gsmStatusMessage(m)
	case len(parts) == 2 && parts[0] == "zigbee" && parts[1] == "status":
		h.zigbeeStatusMessage(m)
	case len(parts) == 2 && parts[0] == "agent" && parts[1] == "status":
		h.agentStatusMessage(m)
	case len(parts) == 2 && parts[0] == "install" && parts[1] == "cmd":
		h.installMessage(m)
	}
}

// envelope parses and checks the envelope of a message, and drops a duplicate.
func (h *Hub) envelope(m Message, wantType string) (contract.Envelope, bool) {
	env, err := contract.ParseEnvelope(m.Payload)
	if err != nil {
		h.log.Warn("ignoring an invalid message", "topic", m.Topic, "err", err)
		return contract.Envelope{}, false
	}
	if env.Site != h.cfg.Site {
		h.log.Warn("ignoring a message for another site", "topic", m.Topic, "site", env.Site)
		return contract.Envelope{}, false
	}
	if env.Type != wantType {
		h.log.Warn("ignoring a message of the wrong type for its topic", "topic", m.Topic, "type", env.Type)
		return contract.Envelope{}, false
	}
	if !h.seen.FirstTime(env.ID) {
		h.log.Debug("ignoring a duplicate", "topic", m.Topic, "id", env.ID)
		return contract.Envelope{}, false
	}
	return env, true
}

// --- devices ----------------------------------------------------------------------

// deviceMessage handles an event, or the retained state of a capability when
// capability is set (the last part of its topic).
func (h *Hub) deviceMessage(deviceID, capability string, m Message) {
	isState := capability != ""
	want := contract.TypeDeviceEvent
	if isState {
		want = contract.TypeDeviceState
	}
	env, ok := h.envelope(m, want)
	if !ok {
		return
	}
	d, err := contract.DecodeData[contract.DeviceEventData](env)
	if err == nil {
		err = d.Validate()
	}
	if err != nil {
		h.log.Warn("ignoring an invalid device message", "device", deviceID, "err", err)
		return
	}
	if d.DeviceID != deviceID || (isState && d.Capability != capability) {
		h.log.Warn("ignoring a device message whose topic and content disagree", "topic", m.Topic, "device", d.DeviceID, "capability", d.Capability)
		return
	}
	if isState {
		h.deviceState(d)
		return
	}
	h.deviceEvent(env.ID, d)
}

// deviceState handles a retained state. They matter once, at start: they tell the
// hub what the devices look like. After that, events carry every change.
func (h *Hub) deviceState(d contract.DeviceEventData) {
	if d.Capability == contract.CapKeypad {
		return
	}
	h.bootMu.Lock()
	defer h.bootMu.Unlock()
	if !h.booting {
		return
	}
	h.bootStates[d.DeviceID+"/"+d.Capability] = d
	h.feedHealth(d)
}

func (h *Hub) deviceEvent(ref string, d contract.DeviceEventData) {
	if d.Capability == contract.CapKeypad {
		h.keypad(ref, d)
		return
	}
	if d.Capability == contract.CapMains {
		h.mains.Store(true)
	}
	if _, ok := signalOf(d); ok {
		err := h.engLoop.Post(h.life, func(now time.Time) []engine.Action {
			sig, _ := engwire.Signal(d, now)
			return h.quietTamper(h.eng.Handle(sig))
		})
		h.submitFailed(err)
	}
	h.feedHealth(d)
}

func signalOf(d contract.DeviceEventData) (engine.Signal, bool) {
	return engwire.Signal(d, time.Time{})
}

func (h *Hub) feedHealth(d contract.DeviceEventData) {
	if _, ok := healthwire.Input(d, time.Time{}); !ok {
		return
	}
	err := h.monLoop.Submit(h.life, func(now time.Time) health.Input {
		in, _ := healthwire.Input(d, now)
		return in
	})
	h.submitFailed(err)
}

func (h *Hub) submitFailed(err error) {
	if err != nil && h.life.Err() == nil {
		h.log.Error("handing an input to a domain failed", "err", err)
	}
}

// --- orders ------------------------------------------------------------------------

var keypadCommands = map[string]string{
	contract.ValArm:      contract.CmdArm,
	contract.ValArmNow:   contract.CmdArmNow,
	contract.ValDisarm:   contract.CmdDisarm,
	contract.ValSilence:  contract.CmdSilence,
	contract.ValPostpone: contract.CmdPostpone,
}

// keypad turns a key sequence from a keypad into an order to the engine.
func (h *Hub) keypad(ref string, d contract.DeviceEventData) {
	name, ok := keypadCommands[d.StringValue()]
	if !ok {
		return
	}
	h.command(ref, contract.AlarmCommandData{
		Command: name, Origin: contract.OriginKeypad,
		Args: contract.CommandArgs{Code: d.Code},
	})
}

func (h *Hub) alarmMessage(m Message) {
	env, ok := h.envelope(m, contract.TypeAlarmCommand)
	if !ok {
		return
	}
	d, err := contract.DecodeData[contract.AlarmCommandData](env)
	if err == nil {
		err = d.Validate()
	}
	if err != nil {
		h.log.Warn("ignoring an invalid order", "err", err)
		return
	}
	h.command(env.ID, d)
}

// authJob is an order whose code is waiting to be checked.
type authJob struct {
	ref string
	cmd contract.AlarmCommandData
}

// command routes an order to the engine. An order that needs a code has it checked
// first, on a worker of its own: the check is deliberately slow and must not hold
// up the siren's schedule or the broker's goroutine.
func (h *Hub) command(ref string, d contract.AlarmCommandData) {
	if !contract.NeedsCode(d.Command) {
		d.Args.Code = ""
		h.submitCommand(ref, d, engine.Auth{})
		return
	}
	if d.Args.Code == "" {
		h.submitCommand(ref, d, engine.Auth{}) // no code checked: the engine counts a wrong one
		return
	}
	select {
	case h.auth <- authJob{ref: ref, cmd: d}:
	default:
		// A flood of code checks: refuse without spending the CPU, and count it as
		// what it is, a wrong code.
		h.log.Warn("too many code checks waiting, refusing this order")
		d.Args.Code = ""
		h.submitCommand(ref, d, engine.Auth{Checked: true})
	}
}

func (h *Hub) authWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-h.auth:
			user, ok := h.codes.Verify(j.cmd.Args.Code)
			if !ok && h.installerCode(j.cmd) {
				h.openInstall(j.ref, j.cmd)
				continue
			}
			j.cmd.Args.Code = "" // the code has done its work
			h.submitCommand(j.ref, j.cmd, engine.Auth{Checked: true, OK: ok, User: user})
		}
	}
}

func (h *Hub) submitCommand(ref string, d contract.AlarmCommandData, auth engine.Auth) {
	err := h.engLoop.Submit(h.life, func(now time.Time) engine.Input {
		return engwire.Command(d, ref, now, auth)
	})
	h.submitFailed(err)
}

// gsmStatusMessage takes the GSM gateway's lines: they go into system.health, and the
// monitor learns whether the SMS backup still works. The gateway repeats its status:
// the monitor notices when it stops (see health.GSM), and the gsm link follows the
// monitor.
func (h *Hub) gsmStatusMessage(m Message) {
	env, ok := h.envelope(m, contract.TypeGSMStatus)
	if !ok {
		return
	}
	d, err := contract.DecodeData[contract.GSMStatusData](env)
	if err != nil {
		h.log.Warn("ignoring an invalid GSM status", "err", err)
		return
	}
	in := healthwire.GSM(d, env.TS.Time)
	h.gsmMu.Lock()
	h.gsm = &contract.GSMHealth{Lines: d.Lines}
	h.gsmMu.Unlock()
	err = h.monLoop.Post(h.life, func(now time.Time) []health.Action {
		in.Now = now
		actions := h.mon.Handle(in)
		h.publishHealth(now)
		return actions
	})
	h.submitFailed(err)
}

// zigbeeStatusMessage takes the Zigbee adapter's status: the monitor learns whether
// the sensors can be heard, and notices when the adapter stops repeating it.
func (h *Hub) zigbeeStatusMessage(m Message) {
	env, ok := h.envelope(m, contract.TypeZigbeeStatus)
	if !ok {
		return
	}
	d, err := contract.DecodeData[contract.ZigbeeStatusData](env)
	if err != nil {
		h.log.Warn("ignoring an invalid Zigbee status", "err", err)
		return
	}
	in := healthwire.Zigbee(d, env.TS.Time)
	err = h.monLoop.Post(h.life, func(now time.Time) []health.Action {
		in.Now = now
		actions := h.mon.Handle(in)
		h.publishHealth(now)
		return actions
	})
	h.submitFailed(err)
}

// agentStatusMessage takes the link agent's status: the monitor learns whether the
// cloud can be reached, and notices when the agent stops repeating it. The internet
// link of system.health follows, and so does the escalation (see publishHealth).
func (h *Hub) agentStatusMessage(m Message) {
	env, ok := h.envelope(m, contract.TypeAgentStatus)
	if !ok {
		return
	}
	d, err := contract.DecodeData[contract.AgentStatusData](env)
	if err != nil {
		h.log.Warn("ignoring an invalid agent status", "err", err)
		return
	}
	in := healthwire.Internet(d, env.TS.Time)
	err = h.monLoop.Post(h.life, func(now time.Time) []health.Action {
		in.Now = now
		actions := h.mon.Handle(in)
		h.publishHealth(now)
		return actions
	})
	h.submitFailed(err)
}
