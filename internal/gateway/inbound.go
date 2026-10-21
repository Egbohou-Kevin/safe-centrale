package gateway

import (
	"time"

	"safehome/internal/contract"
	"safehome/internal/gsm"
	gsmwire "safehome/internal/gsm/wire"
)

// HandleMessage takes a message from the broker. It returns quickly. Payloads are
// never logged: they hold phone texts.
func (g *Gateway) HandleMessage(m Message) {
	if len(m.Payload) > maxPayload {
		return
	}
	site := g.cfg.Site
	var handle func(contract.Envelope)
	switch m.Topic {
	case contract.AlertRequestTopic(site):
		handle = g.alertRequest
	case contract.AlarmEventTopic(site):
		handle = g.alarmEvent
	case contract.AlertAckTopic(site):
		handle = g.alertAck
	default:
		return
	}
	env, err := contract.ParseEnvelope(m.Payload)
	if err != nil || env.Site != site {
		g.log.Warn("ignoring an invalid message", "topic", m.Topic, "err", err)
		return
	}
	handle(env)
}

func (g *Gateway) alertRequest(env contract.Envelope) {
	if env.Type != contract.TypeAlertRequest {
		g.log.Warn("ignoring a message that is not an alert request", "type", env.Type)
		return
	}
	if !g.seen.FirstTime(env.ID) {
		return
	}
	d, err := contract.DecodeData[contract.AlertRequestData](env)
	if err != nil {
		g.log.Warn("ignoring an alert request with a damaged payload", "id", env.ID)
		return
	}
	// Its age counts from its own time: the broker may have held it while the
	// gateway was down.
	r, ok := gsmwire.Request(d, env.ID, env.TS.Time)
	if !ok {
		return // for another channel
	}
	g.submit(func(now time.Time) gsm.Input { r.Now = now; return r })
}

// alarmEvent ends the incident of an alarm a user stopped. The other alarm events
// are not the gateway's business.
func (g *Gateway) alarmEvent(env contract.Envelope) {
	if env.Type != contract.TypeAlarmCleared {
		return
	}
	d, err := contract.DecodeData[contract.ClearedData](env)
	if err != nil {
		g.log.Warn("ignoring an alarm.cleared with a damaged payload", "id", env.ID)
		return
	}
	if c, ok := gsmwire.Cleared(d); ok {
		g.cancel(c)
	}
}

// alertAck ends an incident a contact acknowledged, by SMS (this gateway's own
// acknowledgements come back this way) or in the app.
func (g *Gateway) alertAck(env contract.Envelope) {
	if env.Type != contract.TypeAlertAck {
		return
	}
	d, err := contract.DecodeData[contract.AlertAckData](env)
	if err != nil {
		g.log.Warn("ignoring an alert.ack with a damaged payload", "id", env.ID)
		return
	}
	if c, ok := gsmwire.Acked(d); ok {
		g.cancel(c)
	}
}

func (g *Gateway) cancel(c gsm.Cancel) {
	g.submit(func(now time.Time) gsm.Input { c.Now = now; return c })
}

// Incoming is an SMS a modem received. The composition root wires the modems'
// notifications to it.
func (g *Gateway) Incoming(line, from, text string) {
	g.submit(func(now time.Time) gsm.Input { return gsm.Incoming{Now: now, Line: line, From: from, Text: text} })
}

// Delivered is an operator's delivery report for the SMS with reference ref.
func (g *Gateway) Delivered(line, ref string, ok bool) {
	g.submit(func(now time.Time) gsm.Input { return gsm.DeliveryReport{Now: now, Line: line, MsgRef: ref, OK: ok} })
}

func (g *Gateway) submit(build func(time.Time) gsm.Input) {
	if err := g.loop.Submit(g.life, build); err != nil && g.life.Err() == nil {
		g.log.Error("handing an input to the gateway failed", "err", err)
	}
}
