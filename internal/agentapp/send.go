package agentapp

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"safehome/internal/agent"
	"safehome/internal/contract"
	"safehome/internal/outbox"
)

// ErrRefused is wrapped by the error of the cloud's publisher when its broker refused
// a message for good (not authorized, invalid): the same message would get the same
// answer.
var ErrRefused = errors.New("agentapp: refused by the cloud's broker")

// dispatch carries out what the core decided. It runs on the loop's goroutine.
func (a *App) dispatch(_ context.Context, now time.Time, actions []agent.Action) error {
	for _, act := range actions {
		switch v := act.(type) {
		case agent.PublishLocal, agent.AnswerOrder, agent.Refused, agent.Late:
			a.dispatchDown(now, act)
		case agent.FailAlert:
			a.log.Warn("an alert for the app could not reach the cloud: the hub turns to the GSM", "ref", v.Ref)
			a.publishOwn(now, contract.AlertResultTopic(a.cfg.Site), contract.TypeAlertResult, contract.AlertResultData{
				Ref: v.Ref, Status: contract.ResultFailed, Reason: contract.ReasonNoUplink,
			}, false)
		case agent.StatusChanged:
			if v.Status.Up {
				a.log.Info("connected to the cloud", "transport", v.Status.Transport, "queued", v.Status.Queued)
			} else {
				a.log.Warn("the cloud cannot be reached", "queued", v.Status.Queued)
			}
			// Before the status enters the queue, whose save wakes the sender: the
			// sender takes this signal now, rather than later, where it would cut a
			// retry's wait short.
			a.links.Add(1)
			signal(a.relink)
			a.publishStatus(now, v.Status)
		case agent.Dropped:
			a.log.Error("messages for the cloud were dropped", "count", v.Count, "reason", v.Reason)
		case agent.Replayed:
			a.log.Info("the cloud asked for messages again", "from", v.Report.From, "to", v.Report.To,
				"resent", v.Report.Resent, "missing_ranges", len(v.Report.Missing))
			a.publishOwn(now, contract.AgentReplayedTopic(a.cfg.Site), contract.TypeAgentReplayed, v.Report, false)
		case agent.Restate:
			a.restate(now, v)
		case agent.Upload, agent.RemovePhoto, agent.PhotoReport:
			a.dispatchPhoto(now, act)
		case agent.QueueSave, agent.QueueDrop, agent.UplinkSave:
			// Before the snapshot is saved: see internal/agent/queue.go. An item not
			// saved still goes from memory; it would be lost to a crash.
			if err := a.save(act); err != nil {
				a.log.Error("the queue for the cloud could not be saved", "err", err)
			}
		}
	}
	a.poke()
	return nil
}

// dispatchDown carries out what the core decided about a message from the cloud: pass
// it on to the hub, answer it, or drop it. A message that may carry the access code
// goes at QoS 0: the hub's broker keeps none for a hub away (a restart), neither in
// memory nor in its file on the SD card.
func (a *App) dispatchDown(now time.Time, act agent.Action) {
	switch v := act.(type) {
	case agent.PublishLocal:
		qos := byte(1)
		if v.Code {
			qos = 0
		}
		a.out.Put(&outbox.Item{Topic: v.Topic, Data: v.Payload, QoS: qos, Raw: true})
	case agent.AnswerOrder:
		a.answer(now, v)
	case agent.Refused:
		a.log.Warn("refused a message from the cloud", "topic", v.Topic, "reason", v.Reason)
	case agent.Late:
		a.log.Info("the cloud's word on an alert came after the hub turned to the GSM: dropped",
			"ref", v.Ref, "status", v.Status)
	}
}

// poke wakes the sender: something may be ready for the cloud.
func (a *App) poke() { signal(a.kick) }

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// publishStatus publishes agent.status on the hub, and queues it for the cloud.
func (a *App) publishStatus(now time.Time, st agent.Status) {
	uplink := contract.UplinkDown
	if st.Up {
		uplink = contract.UplinkUp
	}
	a.publishOwn(now, contract.AgentStatusTopic(a.cfg.Site), contract.TypeAgentStatus, contract.AgentStatusData{
		Uplink: uplink, Transport: st.Transport, Queued: st.Queued, OldestS: int(st.Oldest / time.Second), Dropped: st.Dropped,
		Economy: a.cfg.Relay.Economy > 0,
	}, true)
}

// publishOwn publishes a message of the agent on the hub's broker and hands it to the
// core for the cloud: the agent does not hear its own publications (NoLocal). It runs
// on the loop's goroutine.
func (a *App) publishOwn(now time.Time, topic, typ string, data any, retain bool) {
	b, ok := a.message(now, typ, data)
	if !ok {
		return
	}
	a.out.Put(&outbox.Item{Topic: topic, Data: b, QoS: 1, Retain: retain, Raw: true})
	a.handOwn(now, topic, b)
}

// answer answers an order from the app that did not go on to the hub (alarm.ack, ref
// the order's id). For the cloud only: the hub never got the order, so the answer is
// not published on its broker.
func (a *App) answer(now time.Time, v agent.AnswerOrder) {
	a.log.Warn("an order from the app did not go on to the hub: answered to the cloud",
		"ref", v.Ref, "status", v.Status, "reason", v.Reason)
	ack := contract.AlarmAckData{Ref: v.Ref, Status: v.Status, Reason: v.Reason}
	if b, ok := a.message(now, contract.TypeAlarmAck, ack); ok {
		a.handOwn(now, contract.AlarmAckTopic(a.cfg.Site), b)
	}
}

// message builds and encodes a message of the agent.
func (a *App) message(now time.Time, typ string, data any) ([]byte, bool) {
	env, err := contract.NewEnvelope(a.cfg.Site, src, a.seq.Next(), a.deps.NewID(), now, typ, data)
	if err != nil {
		a.log.Error("building a message failed", "type", typ, "err", err)
		return nil, false
	}
	b, err := json.Marshal(env)
	if err != nil {
		a.log.Error("encoding a message failed", "type", typ, "err", err)
		return nil, false
	}
	return b, true
}

// restate hands the core a state of the hub to say again to the cloud, with a new id:
// the cloud drops a known id as a duplicate. The rest of the message is the hub's, its
// ts too. Not published on the hub, which has it already.
func (a *App) restate(now time.Time, r agent.Restate) {
	env, err := contract.ParseEnvelope(r.Payload)
	if err != nil {
		a.log.Error("reading a state to say again failed", "topic", r.Topic, "err", err)
		return
	}
	env.ID = a.deps.NewID()
	b, err := json.Marshal(env)
	if err != nil {
		a.log.Error("encoding a state to say again failed", "topic", r.Topic, "err", err)
		return
	}
	a.handOwn(now, r.Topic, b)
}

// handOwn hands a message of the agent to the core, for the cloud. On the loop: safe,
// and saved with this task. A full queue may answer with actions of its own.
func (a *App) handOwn(now time.Time, topic string, payload []byte) {
	if acts := a.core.Handle(agent.Own{Now: now, Topic: topic, Payload: payload}); len(acts) > 0 {
		a.dispatch(a.life, now, acts)
	}
}

// repeatStatus publishes agent.status, through the core's loop.
func (a *App) repeatStatus(ctx context.Context) {
	a.loop.Do(ctx, func(now time.Time) []agent.Action {
		a.publishStatus(now, a.core.Status(now))
		return nil
	})
}

// sender sends the queue to the cloud, one message at a time, alarms first. A message
// leaves the queue only once the cloud's broker acknowledged it: after a power cut,
// what was not acknowledged goes again (the cloud drops duplicates by id).
func (a *App) sender(ctx context.Context) {
	delay := a.cfg.RetryMin
	for {
		it, ok, seen, err := a.next(ctx)
		if err != nil {
			return
		}
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-a.kick:
			case <-a.relink:
			}
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, a.cfg.SendTimeout)
		err = a.deps.Cloud.Publish(sendCtx, it.Topic, it.Payload, 1, it.Retain)
		cancel()
		if err == nil {
			delay = a.cfg.RetryMin
			a.submit(func(now time.Time) agent.Input { return agent.Sent{Now: now, ID: it.ID} })
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrRefused) {
			env, _ := contract.ParseEnvelope(it.Payload)
			a.log.Error("the cloud's broker refused a message for good: dropped", "type", env.Type, "topic", it.Topic, "err", err)
			a.submit(func(now time.Time) agent.Input { return agent.Rejected{Now: now, ID: it.ID} })
			continue
		}
		if !errors.Is(err, outbox.ErrUnavailable) {
			a.log.Warn("the cloud did not take a message, trying again", "topic", it.Topic, "err", err)
		}
		if !a.wait(ctx, delay, seen) {
			return
		}
		delay = min(delay*2, a.cfg.RetryMax)
	}
}

// next asks the core, on its loop, for the item to send now: none while the link is
// down. An item numbered at this first attempt is saved before Do returns. The answer
// is read only if Do waited for it: at a stop, Do may return while the loop still
// writes it. It also returns how many link changes the answer took into account (see
// wait).
func (a *App) next(ctx context.Context) (agent.Item, bool, uint64, error) {
	var it agent.Item
	var ok bool
	var seen uint64
	err := a.loop.Do(ctx, func(time.Time) []agent.Action {
		seen = a.links.Load()
		if !a.core.Up() {
			return nil
		}
		var save []agent.Action
		it, save, ok = a.core.Next()
		return save
	})
	if err != nil {
		return agent.Item{}, false, 0, err
	}
	return it, ok, seen, nil
}

// wait waits d on the agent's clock, or less if the link changed since the attempt
// was chosen (seen, see next). New messages do not cut it short: the cloud's broker
// refused the last one, and must not be pressed at every new message. Nor does the
// signal of a change the attempt already saw: the loop signals a change on its own
// goroutine, and the sender may take its item after that change before it takes the
// signal; the retry went at once then, and the next wait was twice as long (a test
// waiting for the first retry timed out under -race, 26 October 2026).
func (a *App) wait(ctx context.Context, d time.Duration, seen uint64) bool {
	fired, stop := a.deps.Clock.Timer(d)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-fired:
			return true
		case <-a.relink:
			if a.links.Load() != seen {
				return true
			}
		}
	}
}
