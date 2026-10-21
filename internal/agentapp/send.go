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

// dispatch carries out what the core decided. It runs on the loop's goroutine.
func (a *App) dispatch(_ context.Context, now time.Time, actions []agent.Action) error {
	for _, act := range actions {
		switch v := act.(type) {
		case agent.PublishLocal:
			a.out.Put(&outbox.Item{Topic: v.Topic, Data: v.Payload, QoS: 1, Raw: true})
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
			signal(a.relink)
			a.publishStatus(now, v.Status)
		case agent.Refused:
			a.log.Warn("refused a message from the cloud", "topic", v.Topic, "reason", v.Reason)
		case agent.Dropped:
			a.log.Error("messages for the cloud were dropped", "count", v.Count, "reason", v.Reason)
		case agent.Replayed:
			a.log.Info("the cloud asked for messages again", "from", v.Report.From, "to", v.Report.To,
				"resent", v.Report.Resent, "missing_ranges", len(v.Report.Missing))
			a.publishOwn(now, contract.AgentReplayedTopic(a.cfg.Site), contract.TypeAgentReplayed, v.Report, false)
		case agent.Upload, agent.RemovePhoto, agent.PhotoReport:
			a.dispatchPhoto(now, act)
		case agent.QueueSave, agent.QueueDrop:
			// Before the snapshot is saved: see internal/agent/queue.go. An item not
			// saved still goes from memory; it would be lost to a crash.
			if err := a.storeQueue(act); err != nil {
				a.log.Error("the queue for the cloud could not be saved", "err", err)
			}
		}
	}
	a.poke()
	return nil
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
	env, err := contract.NewEnvelope(a.cfg.Site, src, a.seq.Next(), a.deps.NewID(), now, typ, data)
	if err != nil {
		a.log.Error("building a message failed", "type", typ, "err", err)
		return
	}
	b, err := json.Marshal(env)
	if err != nil {
		a.log.Error("encoding a message failed", "type", typ, "err", err)
		return
	}
	a.out.Put(&outbox.Item{Topic: topic, Data: b, QoS: 1, Retain: retain, Raw: true})
	// On the loop: safe, and saved with this task. A full queue may answer with
	// actions of its own.
	if acts := a.core.Handle(agent.Own{Now: now, Topic: topic, Payload: b}); len(acts) > 0 {
		a.dispatch(a.life, now, acts)
	}
}

// statusRepeater sends the status again every StatusEvery, through the core's loop:
// a loop that hangs stops the repetition, and the hub notices.
func (a *App) statusRepeater(ctx context.Context) {
	for {
		fired, stop := a.deps.Clock.Timer(a.cfg.StatusEvery)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-fired:
			a.loop.Do(ctx, func(now time.Time) []agent.Action {
				a.publishStatus(now, a.core.Status(now))
				return nil
			})
		}
	}
}

// sender sends the queue to the cloud, one message at a time, alarms first. A message
// leaves the queue only once the cloud's broker acknowledged it: after a power cut,
// what was not acknowledged goes again (the cloud drops duplicates by id).
func (a *App) sender(ctx context.Context) {
	delay := a.cfg.RetryMin
	for {
		it, ok, err := a.next(ctx)
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
		if !errors.Is(err, outbox.ErrUnavailable) {
			a.log.Warn("the cloud did not take a message, trying again", "topic", it.Topic, "err", err)
		}
		if !a.wait(ctx, delay) {
			return
		}
		delay = min(delay*2, a.cfg.RetryMax)
	}
}

// next asks the core, on its loop, for the item to send now: none while the link is
// down. An item numbered at this first attempt is saved before Do returns.
func (a *App) next(ctx context.Context) (it agent.Item, ok bool, err error) {
	err = a.loop.Do(ctx, func(time.Time) []agent.Action {
		if !a.core.Up() {
			return nil
		}
		var save []agent.Action
		it, save, ok = a.core.Next()
		return save
	})
	return it, ok, err
}

// wait waits d on the agent's clock, or less if the link changes. New messages do
// not cut it short: the cloud's broker refused the last one, and must not be pressed
// at every new message.
func (a *App) wait(ctx context.Context, d time.Duration) bool {
	fired, stop := a.deps.Clock.Timer(d)
	defer stop()
	select {
	case <-ctx.Done():
		return false
	case <-fired:
	case <-a.relink:
	}
	return true
}
