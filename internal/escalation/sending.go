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
	internet, gsm := split(ct.Channels)
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
		if wave.Double {
			round.GSMDue = at.Add(e.cfg.GSMDelay)
		}
	}
	in.Rounds[ct.ID] = round
	return out
}

func split(chs []Channel) (internet, gsm []Channel) {
	for _, ch := range chs {
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
	_, gsm := split(ct.Channels)
	var out []Action
	for _, ch := range gsm {
		if !slices.Contains(round.Sent, ch) {
			out = append(out, e.send(in, ct, ch, &round, at)...)
		}
	}
	in.Rounds[contact] = round
	return out
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
		Text: e.text(*in, ch), Alarm: in.Alarm, AlarmHandled: in.Quiet,
	}}
}

// allow applies the hourly limit of one contact and counts the message when it
// may go. Fire and panic are never held back, and do not count. Nor is a silent
// siren: it is the one message that says the shop is silent, it must not be held
// back by the alarm's own waves and reminders, and it is bounded on its own (one
// wave, once per siren and per alarm).
func (e *Escalation) allow(kind AlertKind, contact string, at time.Time) bool {
	if kind == KindFire || kind == KindPanic || kind == KindSirenFailed {
		return true
	}
	var recent []time.Time
	for _, t := range e.s.Recent[contact] {
		if at.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) >= e.cfg.MaxPerHour {
		e.s.Recent[contact] = recent
		return false
	}
	e.s.Recent[contact] = append(recent, at)
	return true
}
