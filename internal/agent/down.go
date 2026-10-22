package agent

import (
	"slices"
	"time"

	"safehome/internal/contract"
)

// From the cloud to the hub: what the cloud may send, checked before it reaches the
// hub, and at what pace.

// cloud checks a message from the cloud and passes it to the hub if it may go.
func (r *Relay) cloud(c Cloud) []Action {
	refuse := func(reason string) []Action { return []Action{Refused{Topic: c.Topic, Reason: reason}} }
	site, parts, ok := contract.ParseTopic(c.Topic)
	if !ok || site != r.cfg.Site {
		return refuse("not a topic of this site")
	}
	types, ok := downRoutes[routeKey(parts)]
	if !ok {
		return refuse("the cloud may not publish there")
	}
	env, err := contract.ParseEnvelope(c.Payload)
	switch {
	case err != nil:
		return refuse("invalid message: " + err.Error())
	case env.Site != r.cfg.Site:
		return refuse("message for another site")
	case !slices.Contains(types, env.Type):
		return refuse("wrong type for the topic: " + env.Type)
	case slices.Contains(r.s.Seen, env.ID):
		return nil // a duplicate: already passed on
	case !r.allow(c.Now):
		return refuse("too many messages from the cloud")
	}
	if late, ok := r.lateResult(env); ok {
		r.markSeen(env.ID)
		return []Action{late}
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
	case contract.TypeAlarmCommand:
		return checkAlarmCommand(now, env)
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
	if why == "" && exp != nil && !now.Before(exp.Time) {
		return "expired order"
	}
	return why
}

func checkAlarmCommand(now time.Time, env contract.Envelope) string {
	d, err := contract.DecodeData[contract.AlarmCommandData](env)
	switch {
	case err != nil:
		return err.Error()
	case d.Origin != contract.OriginApp:
		return "an order from the cloud must come from the app, not " + d.Origin
	}
	return orExpired("", now, d.Exp)
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

// allow takes one message from the cloud's allowance, refilled DownPerMinute per
// minute up to DownBurst.
func (r *Relay) allow(now time.Time) bool {
	if !r.s.TokensAt.IsZero() && now.After(r.s.TokensAt) {
		r.s.Tokens += now.Sub(r.s.TokensAt).Minutes() * float64(r.cfg.DownPerMinute)
	}
	r.s.Tokens = min(r.s.Tokens, float64(r.cfg.DownBurst))
	if r.s.TokensAt.IsZero() || now.After(r.s.TokensAt) {
		r.s.TokensAt = now
	}
	if r.s.Tokens < 1 {
		return false
	}
	r.s.Tokens--
	return true
}
