package gateway

import (
	"errors"
	"slices"
	"time"

	"safehome/internal/contract"
	"safehome/internal/gsm"
	gsmwire "safehome/internal/gsm/wire"
	"safehome/internal/inbox"
)

// HandleMessage takes a message from the broker. It returns quickly. Payloads are
// never logged: they hold phone texts.
func (g *Gateway) HandleMessage(m inbox.Message) {
	if len(m.Payload) > maxPayload {
		return
	}
	switch m.Topic {
	case contract.AlertRequestTopic(g.cfg.Site):
		g.alertRequest(m)
	case contract.AlarmEventTopic(g.cfg.Site):
		g.alarmEvent(m)
	case contract.AlertAckTopic(g.cfg.Site):
		g.alertAck(m)
	}
}

func (g *Gateway) alertRequest(m inbox.Message) {
	env, d, err := inbox.Open[contract.AlertRequestData](m, g.cfg.Site, contract.TypeAlertRequest)
	if err != nil {
		g.log.Warn("ignoring an invalid alert request", "id", env.ID, "err", err)
		return
	}
	if !g.seen.FirstTime(env.ID) {
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
// are not the gateway's business. Ending an incident twice changes nothing: no
// duplicate to drop.
func (g *Gateway) alarmEvent(m inbox.Message) {
	env, d, err := inbox.Open[contract.ClearedData](m, g.cfg.Site, contract.TypeAlarmCleared)
	if errors.Is(err, inbox.ErrWrongType) {
		return
	}
	if err != nil {
		g.log.Warn("ignoring an invalid alarm.cleared", "id", env.ID, "err", err)
		return
	}
	if c, ok := gsmwire.Cleared(d); ok {
		g.cancel(c)
	}
}

// alertAck ends an incident a contact acknowledged, by SMS (this gateway's own
// acknowledgements come back this way) or in the app.
func (g *Gateway) alertAck(m inbox.Message) {
	env, d, err := inbox.Open[contract.AlertAckData](m, g.cfg.Site, contract.TypeAlertAck)
	if err != nil {
		g.log.Warn("ignoring an invalid alert.ack", "id", env.ID, "err", err)
		return
	}
	if !g.knows(d.Contact) {
		// The escalation ignores it too, and goes on: the SMS backup must go on with it.
		g.log.Warn("ignoring an alert.ack from someone who is not a contact of the site", "contact", d.Contact)
		return
	}
	if c, ok := gsmwire.Acked(d); ok {
		g.cancel(c)
	}
}

// knows tells whether id is a contact of the site.
func (g *Gateway) knows(id string) bool {
	if g.cfg.Contacts != nil {
		return slices.Contains(g.cfg.Contacts, id)
	}
	_, ok := g.cfg.GSM.Contacts[id]
	return ok
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
