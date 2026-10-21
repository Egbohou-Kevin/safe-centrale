package hub

import (
	"context"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
	"safehome/internal/escalation"
	escwire "safehome/internal/escalation/wire"
	"safehome/internal/health"
	"safehome/internal/inbox"
)

// The hub is where the domains meet: an alert decided by the engine or the monitor
// becomes an input of the escalation, which decides who is told and how. The
// escalation's messages are what leave as alert requests, one per contact and per
// channel. The domains know nothing of each other.

var engineKinds = map[engine.AlertKind]escalation.AlertKind{
	engine.AlertIntrusion:      escalation.KindIntrusion,
	engine.AlertFire:           escalation.KindFire,
	engine.AlertPanic:          escalation.KindPanic,
	engine.AlertTamper:         escalation.KindTamper,
	engine.AlertAuthFailed:     escalation.KindAuthFailed,
	engine.AlertArmingBypassed: escalation.KindArmingBypassed,
}

var engineUrgencies = map[engine.Urgency]escalation.Urgency{
	engine.UrgencyHigh:     escalation.UrgencyHigh,
	engine.UrgencyCritical: escalation.UrgencyCritical,
}

var healthKinds = map[health.AlertKind]escalation.AlertKind{
	health.AlertHubBatteryLow: escalation.KindHubBatteryLow,
	health.AlertMainsRestored: escalation.KindMainsRestored,
	health.AlertDeviceOffline: escalation.KindDeviceOffline,
	health.AlertBatteryLow:    escalation.KindBatteryLow,
	health.AlertGSMDegraded:   escalation.KindGSMDegraded,
	health.AlertZigbeeDown:    escalation.KindZigbeeDown,
}

var healthUrgencies = map[health.Urgency]escalation.Urgency{
	health.UrgencyCritical: escalation.UrgencyCritical,
	health.UrgencyHigh:     escalation.UrgencyHigh,
	health.UrgencyLow:      escalation.UrgencyLow,
}

// escalate hands an input to the escalation loop.
func (h *Hub) escalate(in escalation.Input) {
	err := h.escLoop.Submit(h.life, func(time.Time) escalation.Input { return in })
	h.submitFailed(err)
}

// escalateEngine feeds the engine's alerts to the escalation, and ends the
// escalation of an incident that a person ended (the alarm was disarmed).
func (h *Hub) escalateEngine(now time.Time, actions []engine.Action) {
	for _, a := range actions {
		switch v := a.(type) {
		case engine.Alert:
			kind, okKind := engineKinds[v.Kind]
			urgency, okUrgency := engineUrgencies[v.Urgency]
			if !okKind || !okUrgency {
				h.log.Error("an alert of the engine is unknown to the escalation", "kind", v.Kind, "urgency", v.Urgency)
				continue
			}
			devices := v.Devices
			if len(devices) == 0 && v.Cause != nil {
				devices = []string{v.Cause.DeviceID}
			}
			h.escalate(escalation.Raise{Now: now, Incident: v.Incident, Kind: kind, Urgency: urgency, Devices: devices})
		case engine.Emit:
			if c, ok := v.Event.(engine.Cleared); ok && c.By == engine.ClearedByUser {
				h.escalate(escalation.Close{Now: now, Incident: c.Incident})
			}
		}
	}
}

// escalateHealth feeds the monitor's alerts to the escalation.
func (h *Hub) escalateHealth(now time.Time, actions []health.Action) {
	for _, a := range actions {
		v, ok := a.(health.Alert)
		if !ok {
			continue
		}
		kind, okKind := healthKinds[v.Kind]
		urgency, okUrgency := healthUrgencies[v.Urgency]
		if !okKind || !okUrgency {
			h.log.Error("an alert of the monitor is unknown to the escalation", "kind", v.Kind, "urgency", v.Urgency)
			continue
		}
		h.escalate(escalation.Raise{
			Now: now, Incident: v.Incident, Kind: kind, Urgency: urgency, Devices: v.Devices, Percent: v.Percent,
		})
	}
}

// dispatchEscalation turns the escalation's decisions into alert requests. Each
// message is bound to the id of its request before the request is queued: the
// escalation saves it, so a result finds its message even after a restart.
func (h *Hub) dispatchEscalation(_ context.Context, now time.Time, actions []escalation.Action) error {
	for _, a := range actions {
		switch v := a.(type) {
		case escalation.Send:
			data, err := escwire.Request(v)
			if err != nil {
				h.log.Error("building an alert request failed", "err", err)
				continue
			}
			id := h.deps.NewID()
			h.esc.Bind(v.Key, id)
			h.enqueueAs(id, srcEscalation, contract.AlertRequestTopic(h.cfg.Site), contract.TypeAlertRequest, data, false, now)
		case escalation.Throttled:
			h.log.Warn("a message was held back: the contact already got the hourly maximum",
				"incident", v.Incident, "contact", v.Contact, "channel", v.Channel)
		case escalation.Concluded:
			h.log.Info("escalation over", "incident", v.Incident, "kind", v.Kind, "outcome", v.Outcome, "by", v.By)
		}
	}
	return nil
}

// alertResultMessage handles a channel's report about a message. A result for a
// request the escalation does not know (an old incident) changes nothing.
func (h *Hub) alertResultMessage(m inbox.Message) {
	_, d, ok := decode[contract.AlertResultData](h, m, contract.TypeAlertResult, "alert result")
	if !ok {
		return
	}
	err := h.escLoop.Submit(h.life, func(now time.Time) escalation.Input {
		r, ok := escwire.ResultByRef(d, now)
		if !ok {
			return escalation.Tick{Now: now} // an unknown status says nothing: just let time pass
		}
		return r
	})
	h.submitFailed(err)
}

// alertAckMessage handles a contact's acknowledgement.
func (h *Hub) alertAckMessage(m inbox.Message) {
	_, d, ok := decode[contract.AlertAckData](h, m, contract.TypeAlertAck, "alert acknowledgement")
	if !ok {
		return
	}
	err := h.escLoop.Submit(h.life, func(now time.Time) escalation.Input { return escwire.Ack(d, now) })
	h.submitFailed(err)
}
