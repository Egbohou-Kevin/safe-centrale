package agent

import (
	"slices"
	"time"

	"safehome/internal/contract"
)

// From the cloud to the hub: what the cloud may send, checked before it reaches the
// hub, and at what pace. An order from the app that passed the checks but does not
// go on to the hub (too late, or over the limit) is answered to the cloud at once.

// cloud checks a message from the cloud and passes it to the hub if it may go.
func (r *Relay) cloud(c Cloud) []Action {
	refuse := func(reason string) []Action { return []Action{Refused{Topic: c.Topic, Reason: reason}} }
	site, parts, ok := contract.ParseTopic(c.Topic)
	if !ok || site != r.cfg.Site {
		return refuse("not a topic of this site")
	}
	route, ok := downRoutes[routeKey(parts)]
	if !ok {
		return refuse("the cloud may not publish there")
	}
	env, err := contract.ParseEnvelope(c.Payload)
	switch {
	case err != nil:
		return refuse("invalid message: " + err.Error())
	case env.Site != r.cfg.Site:
		return refuse("message for another site")
	case !slices.Contains(route.types, env.Type):
		return refuse("wrong type for the topic: " + env.Type)
	case slices.Contains(r.s.Seen, env.ID):
		return nil // a duplicate: already passed on, or answered
	case !r.allow(c.Now):
		return r.overLimit(c, env)
	}
	if late, ok := r.lateResult(env); ok {
		r.markSeen(env.ID)
		return []Action{late}
	}
	if env.Type == contract.TypeAlarmCommand {
		return r.passOrder(c, env)
	}
	if reason := r.check(c.Now, env); reason != "" {
		return refuse(reason)
	}
	r.markSeen(env.ID)
	switch env.Type {
	case contract.TypePhotoUpload:
		return nil // for the agent itself: its address never reaches the hub
	case contract.TypeAgentReplay:
		d, _ := contract.DecodeData[contract.ReplayData](env) // checked above
		return r.replayRange(d)
	}
	return []Action{PublishLocal{Topic: c.Topic, Payload: c.Payload}}
}

// markSeen keeps the id of a message passed on, to drop it if it comes again.
func (r *Relay) markSeen(id string) {
	r.s.Seen = append(r.s.Seen, id)
	if over := len(r.s.Seen) - r.cfg.SeenSize; over > 0 {
		r.s.Seen = slices.Delete(r.s.Seen, 0, over)
	}
}

// check applies the rules of each type. It returns why the message is refused, or
// "".
func (r *Relay) check(now time.Time, env contract.Envelope) string {
	switch env.Type {
	case contract.TypeVideoRequest:
		d, why := valid[contract.VideoRequestData](env)
		return orExpired(why, now, &d.Exp)
	case contract.TypeVideoStop:
		_, why := valid[contract.VideoStopData](env)
		return why
	case contract.TypeMaintOpen:
		d, why := valid[contract.MaintOpenData](env)
		return orExpired(why, now, &d.Exp)
	case contract.TypeMaintClose:
		_, why := valid[contract.MaintCloseData](env)
		return why
	case contract.TypeAgentReplay:
		_, why := valid[contract.ReplayData](env)
		return why
	case contract.TypeConfigSet:
		_, why := valid[contract.ConfigSetData](env)
		return why
	case contract.TypePhotoUpload:
		return r.photoUpload(now, env)
	case contract.TypeAlertResult:
		return r.alertResult(env)
	case contract.TypeAlertAck:
		return r.alertAck(env)
	}
	return ""
}

// valid decodes data that checks its own shape. It returns why it is refused, or "".
func valid[T any](env contract.Envelope) (T, string) {
	d, err := contract.ValidData[T](env)
	if err != nil {
		return d, err.Error()
	}
	return d, ""
}

// orExpired returns why, or that the order expired: an order seen after exp is
// refused.
func orExpired(why string, now time.Time, exp *contract.Timestamp) string {
	if why == "" && expired(now, exp) {
		return "expired order"
	}
	return why
}

func expired(now time.Time, exp *contract.Timestamp) bool { return exp != nil && !now.Before(exp.Time) }

// appOrder reads an order from the cloud: it must come from the app. It returns why
// it is refused, or "".
func appOrder(env contract.Envelope) (contract.AlarmCommandData, string) {
	d, err := contract.DecodeData[contract.AlarmCommandData](env)
	switch {
	case err != nil:
		return d, err.Error()
	case d.Origin != contract.OriginApp:
		return d, "an order from the cloud must come from the app, not " + d.Origin
	}
	return d, ""
}

// passOrder passes an order from the app on to the hub. One that came too late is
// answered expired at once, as the engine would answer it: the cloud need not wait
// for an answer that cannot come. Its code goes nowhere.
func (r *Relay) passOrder(c Cloud, env contract.Envelope) []Action {
	d, why := appOrder(env)
	if why != "" {
		return []Action{Refused{Topic: c.Topic, Reason: why}}
	}
	r.markSeen(env.ID) // passed on or answered: once either way
	if expired(c.Now, d.Exp) {
		return []Action{AnswerOrder{Ref: env.ID, Status: contract.AckExpired}}
	}
	return []Action{PublishLocal{Topic: c.Topic, Payload: c.Payload}}
}

// overLimit refuses a message over the cloud's allowance. An order from the app that
// passes the checks is answered refused (rate_limited), so that the phone knows at
// once, and is remembered: given again later, it must not run after that answer. The
// answers draw on an allowance of their own, as large as the orders': a flood gets no
// more of them than orders would pass (each one is a write to the SD card), the rest
// is dropped as before, and the orders' allowance is not touched.
func (r *Relay) overLimit(c Cloud, env contract.Envelope) []Action {
	if env.Type == contract.TypeAlarmCommand {
		if _, why := appOrder(env); why == "" && r.take(c.Now, &r.s.Answers, &r.s.AnswersAt) {
			r.markSeen(env.ID)
			return []Action{AnswerOrder{Ref: env.ID, Status: contract.AckRefused, Reason: contract.ReasonRateLimited}}
		}
	}
	return []Action{Refused{Topic: c.Topic, Reason: "too many messages from the cloud"}}
}

// alertResult takes the cloud's word on an alert the agent sent it.
func (r *Relay) alertResult(env contract.Envelope) string {
	d, err := contract.DecodeData[contract.AlertResultData](env)
	if err != nil {
		return err.Error()
	}
	p, ok := r.s.Pending[d.Ref]
	if !ok {
		return "result for an alert the agent did not send to the cloud"
	}
	switch d.Status {
	case contract.ResultQueued, contract.ResultSent:
		p.Confirmed, p.Deadline = true, time.Time{}
		r.s.Pending[d.Ref] = p
	case contract.ResultDelivered, contract.ResultFailed:
		delete(r.s.Pending, d.Ref) // the last word on it
	default:
		return "unknown status " + d.Status
	}
	return ""
}

// lateResult tells whether a message is the cloud's word on an alert the agent gave
// up on: the hub already turned to the GSM, so it never reaches the hub.
func (r *Relay) lateResult(env contract.Envelope) (Late, bool) {
	if env.Type != contract.TypeAlertResult {
		return Late{}, false
	}
	d, err := contract.DecodeData[contract.AlertResultData](env)
	if err != nil || r.s.GivenUp[d.Ref].IsZero() {
		return Late{}, false
	}
	return Late{Ref: d.Ref, Status: d.Status}, true
}

// alertAck checks an acknowledgement from the app: only for an incident the cloud
// was told about.
func (r *Relay) alertAck(env contract.Envelope) string {
	d, err := contract.DecodeData[contract.AlertAckData](env)
	switch {
	case err != nil:
		return err.Error()
	case d.Via != contract.ChannelApp:
		return "an acknowledgement from the cloud comes from the app, not " + d.Via
	case d.Incident == "" || r.s.Incidents[d.Incident].IsZero():
		return "acknowledgement of an incident the cloud was not told about"
	}
	return ""
}

// allow takes one message from the cloud's allowance.
func (r *Relay) allow(now time.Time) bool { return r.take(now, &r.s.Tokens, &r.s.TokensAt) }

// take takes one from an allowance (what is left, and when it was last refilled),
// refilled DownPerMinute per minute up to DownBurst. A refusal changes neither: the
// allowance it found is worked out again at the next message, and a flood over the
// limit causes no write of the snapshot.
func (r *Relay) take(now time.Time, tokens *float64, at *time.Time) bool {
	avail := *tokens
	if !at.IsZero() && now.After(*at) {
		avail += now.Sub(*at).Minutes() * float64(r.cfg.DownPerMinute)
	}
	avail = min(avail, float64(r.cfg.DownBurst))
	if avail < 1 {
		return false
	}
	*tokens = avail - 1
	if at.IsZero() || now.After(*at) {
		*at = now
	}
	return true
}
