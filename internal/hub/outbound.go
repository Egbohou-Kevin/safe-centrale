package hub

import (
	"context"
	"maps"
	"slices"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
	engwire "safehome/internal/engine/wire"
	"safehome/internal/escalation"
	"safehome/internal/health"
)

// dispatchEngine turns what the engine decided into messages. It runs on the
// engine loop's goroutine and only queues: the sending is done elsewhere.
//
// The siren leaves first, then the alert requests, then the events: if the broker
// is slow, what matters most for people leaves first.
func (h *Hub) dispatchEngine(_ context.Context, now time.Time, actions []engine.Action) error {
	out, err := engwire.Translate(actions)
	if err != nil {
		return err
	}
	site := h.cfg.Site
	for _, s := range out.Siren {
		for _, id := range h.cfg.SirenIDs {
			h.enqueue(srcEngine, contract.DeviceCmdTopic(site, id), contract.TypeCommand,
				s.Data(id, now.Add(h.cfg.SirenCommandTTL)), 1, false, now)
		}
	}
	// The engine's alerts do not leave as they are: the escalation turns each into one
	// request per contact and channel.
	h.escalateEngine(now, actions)
	h.warnOfFaults(&out)
	for _, ev := range out.Events {
		h.enqueue(srcEngine, contract.AlarmEventTopic(site), ev.Type, ev.Data, 1, false, now)
	}
	for _, ack := range out.Acks {
		h.enqueue(srcEngine, contract.AlarmAckTopic(site), contract.TypeAlarmAck, ack, 1, false, now)
	}
	return nil
}

// warnOfFaults adds the faults the monitor knows to an arming that starts: the event
// lists them, and so does the reply to the arming order, for the keypad to warn.
// Arming is never refused for them (docs/engine.md). One input gives at most one
// reply, so a reply next to the arming event answers the order that started it.
func (h *Hub) warnOfFaults(out *engwire.Out) {
	st := h.faults.Load()
	if len(st.Offline) == 0 && len(st.LowBattery) == 0 {
		return
	}
	arming := false
	for i, ev := range out.Events {
		if d, ok := ev.Data.(contract.ArmingData); ok {
			d.Offline, d.LowBattery = st.Offline, st.LowBattery
			out.Events[i].Data = d
			arming = true
		}
	}
	if !arming {
		return
	}
	for i, ack := range out.Acks {
		if ack.Status != contract.AckDone {
			continue
		}
		detail := maps.Clone(ack.Detail)
		if detail == nil {
			detail = map[string]any{}
		}
		if len(st.Offline) > 0 {
			detail["offline"] = st.Offline
		}
		if len(st.LowBattery) > 0 {
			detail["low_battery"] = st.LowBattery
		}
		out.Acks[i].Detail = detail
	}
}

// publishState publishes the retained alarm state, and keeps the heartbeat's copy.
func (h *Hub) publishState(now time.Time) {
	st := h.eng.State(now)
	h.mode.Store(string(st.Mode))
	data, err := engwire.StateData(st)
	if err != nil {
		h.log.Error("building the alarm state failed", "err", err)
		return
	}
	h.enqueue(srcEngine, contract.AlarmStateTopic(h.cfg.Site), contract.TypeAlarmState, data, 1, true, now)
}

// dispatchHealth turns the monitor's alerts into alert requests.
func (h *Hub) dispatchHealth(_ context.Context, now time.Time, actions []health.Action) error {
	h.escalateHealth(now, actions)
	return nil
}

// publishHealth publishes the retained system.health.
func (h *Hub) publishHealth(now time.Time) {
	st := h.mon.Status()
	h.faults.Store(&st)
	h.followUplink(now, st.Internet)
	data := contract.HealthData{Links: map[string]string{}}
	for name, s := range map[string]health.LinkState{linkGSM: st.GSM, linkZigbee: st.Zigbee, linkInternet: st.Internet} {
		if link := linkOf(s); link != "" {
			data.Links[name] = link
		}
	}
	h.gsmMu.Lock()
	if h.gsm != nil {
		data.GSM = &contract.GSMHealth{Lines: slices.Clone(h.gsm.Lines)}
	}
	h.gsmMu.Unlock()
	if h.mains.Load() {
		data.Mains = contract.ValPresent
		if st.MainsLost {
			data.Mains = contract.ValLost
		}
	}
	if st.HubBatteryKnown {
		b := st.HubBattery
		data.HubBattery = &b
	}
	h.enqueue(srcHealth, contract.HealthTopic(h.cfg.Site), contract.TypeHealth, data, 1, true, now)
}

// linkOf is a link of system.health for a component the monitor watches (the GSM
// gateway, the Zigbee adapter): empty while it never spoke.
func linkOf(s health.LinkState) string {
	switch s {
	case health.LinkOK:
		return linkOK
	case health.LinkDown:
		return linkDown
	case health.LinkSilent:
		return linkSilent
	}
	return ""
}

// followUplink tells the escalation when the internet comes and goes: without it, the
// GSM channels take over at once instead of waiting for a confirmation that cannot
// come. Called from the monitor's loop, which owns uplink.
func (h *Hub) followUplink(now time.Time, s health.LinkState) {
	if s == h.uplink {
		return
	}
	h.uplink = s
	if s == health.LinkUnknown {
		return // the agent never spoke: nothing to say yet
	}
	h.escalate(escalation.Uplink{Now: now, Up: s == health.LinkOK})
}
