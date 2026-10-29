package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
	engwire "safehome/internal/engine/wire"
	"safehome/internal/escalation"
	"safehome/internal/health"
)

// Sources written in the envelope.
const (
	srcEngine     = "engine"
	srcHealth     = "health"
	srcEscalation = "escalation"
	srcInstall    = "install"
)

// The link names and states of system.health.
const (
	linkInternet = "internet"
	linkGSM      = "gsm"
	linkZigbee   = "zigbee"
	linkOK       = "ok"
	linkDown     = "down"
	linkSilent   = "silent" // the component stopped giving news (it is dead or stuck)
	linkNone     = "none"   // the site has none, by choice: a hub without a GSM line
)

// dispatchEngine turns what the engine decided into messages. It runs on the
// engine loop's goroutine and only queues: the sending is done elsewhere.
//
// A site without a siren (Config.NoSiren) tells no siren cycle: the engine's schedule
// still times the alarm (its end, alarm.cleared by timeout), but nothing sounds, and
// the history must not say it does.
//
// The siren leaves first, then the events and the replies: if the broker is slow,
// what matters most for people leaves first. The alerts go to the escalation, whose
// own loop queues the alert requests a moment later, after the siren.
func (h *Hub) dispatchEngine(_ context.Context, now time.Time, actions []engine.Action) error {
	out, err := engwire.Translate(actions)
	if err != nil {
		return err
	}
	h.orderSirens(now, actions)
	// The engine's alerts do not leave as they are: the escalation turns each into one
	// request per contact and channel.
	h.escalateEngine(now, actions)
	h.warnOfFaults(&out)
	for _, ev := range out.Events {
		if ev.Type == contract.TypeAlarmSirenCycle && h.cfg.NoSiren() {
			continue // a site without a siren: the engine's schedule times the alarm, nothing sounds
		}
		h.out.Send(srcEngine, contract.AlarmEventTopic(h.cfg.Site), ev.Type, ev.Data, false, now)
	}
	for _, ack := range out.Acks {
		h.out.Send(srcEngine, contract.AlarmAckTopic(h.cfg.Site), contract.TypeAlarmAck, ack, false, now)
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
	st := h.alarm.eng.State(now)
	h.mode.Store(string(st.Mode))
	data, err := engwire.StateData(st)
	if err != nil {
		h.log.Error("building the alarm state failed", "err", err)
		return
	}
	h.out.Send(srcEngine, contract.AlarmStateTopic(h.cfg.Site), contract.TypeAlarmState, data, true, now)
}

// dispatchHealth turns the monitor's alerts into alert requests.
func (h *Hub) dispatchHealth(_ context.Context, now time.Time, actions []health.Action) error {
	h.escalateHealth(now, actions)
	return nil
}

// publishHealth publishes the retained system.health when what it says changed: the
// services repeat their status every minute, and each copy would go to the cloud. It
// runs on the monitor's loop.
func (h *Hub) publishHealth(now time.Time) {
	st := h.monitor.mon.Status()
	h.faults.Store(&st)
	h.followUplink(now, st.Internet)
	h.publishChanged(&h.monitor.lastHealth, contract.HealthTopic(h.cfg.Site), contract.TypeHealth, h.healthData(st), now)
}

// publishChanged publishes a retained message of the monitor when it differs from the
// last one published there (*last), and keeps it. It runs on the monitor's loop.
func (h *Hub) publishChanged(last *[]byte, topic, typ string, data any, now time.Time) {
	raw, err := json.Marshal(data)
	if err != nil {
		h.log.Error("encoding a message failed", "type", typ, "err", err)
		return
	}
	if bytes.Equal(raw, *last) {
		return
	}
	*last = raw
	h.out.Send(srcHealth, topic, typ, json.RawMessage(raw), true, now)
}

// healthData is system.health for the monitor's summary and the GSM lines.
func (h *Hub) healthData(st health.Status) contract.HealthData {
	data := contract.HealthData{Links: map[string]string{}, GSM: h.gsm.Load()}
	for name, s := range map[string]health.LinkState{linkGSM: st.GSM, linkZigbee: st.Zigbee, linkInternet: st.Internet} {
		if link := linkOf(s); link != "" {
			data.Links[name] = link
		}
	}
	if st.WithoutPower { // no module, by choice: never a fault, and no battery to tell
		data.Mains = contract.MainsNone
		return data
	}
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
	return data
}

// linkOf is a link of system.health for a component the monitor watches (the GSM
// gateway, the Zigbee adapter, the link agent): empty while it never spoke.
func linkOf(s health.LinkState) string {
	switch s {
	case health.LinkOK:
		return linkOK
	case health.LinkDown:
		return linkDown
	case health.LinkSilent:
		return linkSilent
	case health.LinkNone:
		return linkNone
	}
	return ""
}

// followUplink tells the escalation when the internet comes and goes: without it, the
// GSM channels take over at once instead of waiting for a confirmation that cannot
// come. Called from the monitor's loop, which owns uplink.
func (h *Hub) followUplink(now time.Time, s health.LinkState) {
	if s == h.monitor.uplink {
		return
	}
	h.monitor.uplink = s
	if s == health.LinkUnknown {
		return // the agent never spoke: nothing to say yet
	}
	h.escalate(escalation.Uplink{Now: now, Up: s == health.LinkOK})
}

// publishHeartbeat publishes a sign of life (every HeartbeatEvery).
func (h *Hub) publishHeartbeat(context.Context) {
	now := h.deps.Clock.Now()
	mode, _ := h.mode.Load().(string)
	data := contract.HeartbeatData{
		BootID:     h.bootID,
		UptimeS:    max(int64(now.Sub(h.started)/time.Second), 0),
		Engine:     mode,
		QueueDepth: h.out.Depth(),
		FW:         h.cfg.Versions,
	}
	if h.deps.ClockSynced != nil {
		if synced, known := h.deps.ClockSynced(); known {
			data.ClockOK = &synced
		}
	}
	h.out.Send(srcEngine, contract.HeartbeatTopic(h.cfg.Site), contract.TypeHeartbeat, data, false, now)
}
