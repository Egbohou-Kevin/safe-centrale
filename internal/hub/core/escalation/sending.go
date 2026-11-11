package escalation

import (
	"slices"
	"time"
)

// notify tells one contact for one step: internet first, the GSM channels when the
// internet is unusable or does not confirm, and again to insist.
func (e *Escalation) notify(in *IncidentSnap, ct Contact, step int, wave Wave, at time.Time) []Action {
	plan := e.plan(*in)
	round := Round{Step: step, Started: at}
	internet, gsm := e.split(ct)
	var out []Action
	send := func(chs ...Channel) {
		for _, ch := range chs {
			out = append(out, e.send(in, ct, ch, &round, at)...)
		}
	}

	switch {
	case plan.InternetOnly:
		if len(internet) > 0 {
			if e.s.UplinkDown {
				round.NetHeld = true
			} else {
				send(internet...)
			}
		}
		if plan.GSMAfter > 0 && len(gsm) > 0 {
			round.GSMDue = at.Add(plan.GSMAfter)
		}
	case len(internet) == 0 || e.s.UplinkDown:
		round.NetHeld = len(internet) > 0
		send(gsm...) // the internet cannot be used: the GSM channels take over at once
	default:
		send(internet...)
		round.NetDeadline = at.Add(e.cfg.InternetConfirm)
		if wave.SMSNow && slices.Contains(gsm, ChannelSMS) {
			send(ChannelSMS)
		}
		if wave.Double && gsmLeft(gsm, round) {
			round.GSMDue = at.Add(e.cfg.GSMDelay)
		}
	}
	in.Rounds[ct.ID] = round
	return out
}

// split sorts the channels of a contact into the internet ones and the GSM ones. A
// site without a GSM line (Config.WithoutGSM) has none of the latter: no SMS and no
// call is ever asked for there, whatever the contact accepts.
func (e *Escalation) split(ct Contact) (internet, gsm []Channel) {
	for _, ch := range e.cfg.usable(ct.Channels) {
		if ch.internet() {
			internet = append(internet, ch)
		} else {
			gsm = append(gsm, ch)
		}
	}
	return internet, gsm
}

// gsm sends, to one contact, the GSM channels not yet used in the current round.
func (e *Escalation) gsm(in *IncidentSnap, contact string, at time.Time) []Action {
	ct, ok := e.cfg.contact(contact)
	if !ok {
		return nil
	}
	round := in.Rounds[contact]
	_, gsm := e.split(ct)
	var out []Action
	for _, ch := range gsm {
		if !slices.Contains(round.Sent, ch) {
			out = append(out, e.send(in, ct, ch, &round, at)...)
		}
	}
	if !gsmLeft(gsm, round) {
		// Nothing left for a GSM takeover to send: waiting for it would only hold the
		// next wave back (see pullForwardIfAllUnreachable) and the end of the incident.
		round.GSMDue = time.Time{}
	}
	in.Rounds[contact] = round
	return out
}

// gsmLeft tells whether a GSM channel of the round is still unused.
func gsmLeft(gsm []Channel, round Round) bool {
	return slices.ContainsFunc(gsm, func(ch Channel) bool { return !slices.Contains(round.Sent, ch) })
}

// resend sends a channel again in the current round (the round's channel list
// already holds it).
func (e *Escalation) resend(in *IncidentSnap, ct Contact, ch Channel, round *Round, at time.Time) []Action {
	round.Sent = slices.DeleteFunc(round.Sent, func(c Channel) bool { return c == ch })
	return e.send(in, ct, ch, round, at)
}

// send builds one message, unless the contact already got the hourly maximum. An
// alert of an alarm already handled never rings anyone: its call is withheld, and
// counts as used for the round so that it is reported once.
func (e *Escalation) send(in *IncidentSnap, ct Contact, ch Channel, round *Round, at time.Time) []Action {
	if in.Quiet && ch == ChannelCall {
		if !slices.Contains(round.Sent, ch) {
			round.Sent = append(round.Sent, ch)
		}
		return []Action{Withheld{Incident: in.ID, Alarm: in.Alarm, Contact: ct.ID, Channel: ch}}
	}
	if !e.allow(in.Kind, ct.ID, at) {
		return []Action{Throttled{Incident: in.ID, Contact: ct.ID, Channel: ch}}
	}
	e.s.NextKey++
	key := e.s.NextKey
	in.Deliveries[key] = Delivery{Contact: ct.ID, Channel: ch, Step: round.Step}
	if !slices.Contains(round.Sent, ch) {
		round.Sent = append(round.Sent, ch)
	}
	urgency := in.Urgency
	if in.Quiet {
		urgency = UrgencyLow // nothing is urgent any more; it decides nothing either
	}
	return []Action{Send{
		Key: key, Incident: in.ID, RaisedAt: in.RaisedAt, Kind: in.Kind, Urgency: urgency, Wave: round.Step + 1,
		Contact: ct.ID, Channel: ch, Devices: slices.Clone(in.Devices), Percent: in.Percent,
		Text: e.text(*in, ch), Alarm: in.Alarm, AlarmHandled: in.Quiet, ZoneName: e.zoneName(*in),
	}}
}

// zoneName names the zone of an alert's cause: one device, in a zone, for the kinds
// a device in a zone raises. Several devices, or a fault of the hub, name none.
func (e *Escalation) zoneName(in IncidentSnap) string {
	switch in.Kind {
	case KindIntrusion, KindFire, KindPanic, KindTamper:
		if len(in.Devices) == 1 {
			return e.cfg.ZoneNames[in.Devices[0]]
		}
	}
	return ""
}

// allow applies the hourly limit of one contact and counts the message when it
// may go. Fire and panic are never held back, and do not count. Nor is a silent
// siren: it is the one message that says the shop is silent, it must not be held
// back by the alarm's own waves and reminders, and it is bounded on its own (one
// wave, once per siren and per alarm). An intrusion may use the whole limit; the other
// alerts (a tamper, wrong codes, the faults) half of it at most, counted apart too: a
// keypad tried every ten minutes, or covers opened one after the other, must not use
// up the messages the intrusion that follows needs to reach the owner.
func (e *Escalation) allow(kind AlertKind, contact string, at time.Time) bool {
	if kind == KindFire || kind == KindPanic || kind == KindSirenFailed {
		return true
	}
	recent := lastHour(e.s.Recent, contact, at)
	lesser := lastHour(e.s.Lesser, contact, at)
	if len(recent) >= e.cfg.MaxPerHour || kind != KindIntrusion && len(lesser) >= max(e.cfg.MaxPerHour/2, 1) {
		return false
	}
	e.s.Recent[contact] = append(recent, at)
	if kind != KindIntrusion {
		e.s.Lesser[contact] = append(lesser, at)
	}
	return true
}

// lastHour drops from sent the messages to contact older than an hour, and returns
// those left.
func lastHour(sent map[string][]time.Time, contact string, at time.Time) []time.Time {
	var recent []time.Time
	for _, t := range sent[contact] {
		if at.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(sent, contact)
	} else {
		sent[contact] = recent
	}
	return recent
}
