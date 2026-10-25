package hub

import (
	"errors"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
	engwire "safehome/internal/engine/wire"
	"safehome/internal/health"
	healthwire "safehome/internal/health/wire"
	"safehome/internal/inbox"
)

const maxPayload = 64 * 1024

// HandleMessage takes a message from the broker. It is meant to be called from the
// MQTT client's own goroutine and returns quickly: it decodes, checks, and hands
// the work to the domain loops. A message that is not ours, or is damaged, is
// dropped with a log line that never contains the payload (it may hold a code).
func (h *Hub) HandleMessage(m inbox.Message) {
	if len(m.Payload) > maxPayload {
		h.log.Warn("ignoring an oversized message", "topic", m.Topic, "bytes", len(m.Payload))
		return
	}
	site, parts, ok := contract.ParseTopic(m.Topic)
	if !ok || site != h.cfg.Site {
		return
	}
	switch {
	case len(parts) == 3 && parts[0] == "device" && parts[2] == "event":
		h.deviceMessage(parts[1], "", m)
	case len(parts) == 3 && parts[0] == "device" && parts[2] == "ack":
		h.sirenAckMessage(parts[1], m)
	case len(parts) == 4 && parts[0] == "device" && parts[2] == "state":
		h.deviceMessage(parts[1], parts[3], m)
	case len(parts) == 2:
		h.serviceMessage(parts[0]+"/"+parts[1], m)
	}
}

// serviceMessage routes a message on a topic of the site's services, named by the
// last two parts of its topic.
func (h *Hub) serviceMessage(topic string, m inbox.Message) {
	switch topic {
	case "alarm/cmd":
		h.alarmMessage(m)
	case "alert/result":
		h.alertResultMessage(m)
	case "alert/ack":
		h.alertAckMessage(m)
	case "alert/seen":
		h.alertSeenMessage(m)
	case "gsm/status":
		h.gsmStatusMessage(m)
	case "zigbee/status":
		h.zigbeeStatusMessage(m)
	case "agent/status":
		h.agentStatusMessage(m)
	case "install/cmd":
		h.installMessage(m)
	case "registry/event":
		h.registryMessage(m)
	case "registry/devices":
		h.registrySnapshotMessage(m)
	}
}

// decode reads a message of type typ through the inbox's gate, and drops a
// duplicate. A message refused is logged as what it should have been, never with its
// payload.
func decode[T any](h *Hub, m inbox.Message, typ, what string) (contract.Envelope, T, bool) {
	env, d, err := inbox.Open[T](m, h.cfg.Site, typ)
	if err != nil {
		h.log.Warn("ignoring an invalid "+what, "topic", m.Topic, "err", err)
		return env, d, false
	}
	if !h.seen.FirstTime(env.ID) {
		h.log.Debug("ignoring a duplicate", "topic", m.Topic, "id", env.ID)
		return env, d, false
	}
	return env, d, true
}

// submitFailed logs an input a loop did not take, unless the hub is stopping.
func (h *Hub) submitFailed(err error) {
	if err != nil && h.life.Err() == nil {
		h.log.Error("handing an input to a domain failed", "err", err)
	}
}

// --- devices ----------------------------------------------------------------------

// deviceMessage handles an event, or the retained state of a capability when
// capability is set (the last part of its topic).
func (h *Hub) deviceMessage(deviceID, capability string, m inbox.Message) {
	isState := capability != ""
	typ := contract.TypeDeviceEvent
	if isState {
		typ = contract.TypeDeviceState
	}
	env, d, ok := decode[contract.DeviceEventData](h, m, typ, "device message")
	if !ok {
		return
	}
	if d.DeviceID != deviceID || (isState && d.Capability != capability) {
		h.log.Warn("ignoring a device message whose topic and content disagree", "topic", m.Topic, "device", d.DeviceID, "capability", d.Capability)
		return
	}
	if isState {
		h.deviceState(d)
		return
	}
	h.deviceEvent(env, d)
}

// deviceState handles a retained state. They matter once, at start: they tell the
// hub what the devices look like. After that, events carry every change, the
// availability apart: its state also says since when a device is offline and when it
// was last heard, which its event does not, so the monitor follows it by its state
// alone, at start and after (system.devices).
func (h *Hub) deviceState(d contract.DeviceEventData) {
	if d.Capability == contract.CapKeypad {
		return
	}
	h.startup.mu.Lock()
	booting := h.startup.open
	if booting {
		h.startup.states[d.DeviceID+"/"+d.Capability] = d
	}
	h.startup.mu.Unlock()
	if !booting && d.Capability != contract.CapAvailability {
		return
	}
	if d.Capability == contract.CapMains {
		h.mains.Store(true)
	}
	h.feedHealth(d)
}

func (h *Hub) deviceEvent(env contract.Envelope, d contract.DeviceEventData) {
	if d.Capability == contract.CapKeypad {
		h.keypad(env, d)
		return
	}
	if d.Capability == contract.CapMains {
		h.mains.Store(true)
	}
	if _, ok := signalOf(d); ok {
		err := h.alarm.loop.Post(h.life, func(now time.Time) []engine.Action {
			sig, _ := engwire.Signal(d, now)
			if h.quietTamper(sig) {
				return nil
			}
			return h.alarm.eng.Handle(sig)
		})
		h.submitFailed(err)
	}
	if d.Capability != contract.CapAvailability { // followed by its state (deviceState)
		h.feedHealth(d)
	}
}

// signalOf tells whether a device message means something to the engine; the time
// is set when the signal is handed over.
func signalOf(d contract.DeviceEventData) (engine.Signal, bool) {
	return engwire.Signal(d, time.Time{})
}

// retainedSignal is the signal of a retained state, dated when it began (since): the
// engine gives no credit to one older than what it heard before the restart (see
// engine.Boot).
func retainedSignal(d contract.DeviceEventData) (engine.Signal, bool) {
	sig, ok := signalOf(d)
	if d.Since != nil {
		sig.Now = d.Since.Time
	}
	return sig, ok
}

func (h *Hub) feedHealth(d contract.DeviceEventData) {
	if _, ok := healthwire.Input(d, time.Time{}); !ok {
		return
	}
	err := h.monitor.loop.Submit(h.life, func(now time.Time) health.Input {
		in, _ := healthwire.Input(d, now)
		return in
	})
	h.submitFailed(err)
}

// registryMessage takes a device removed from the site: the engine and the monitor
// forget it, so that it holds no door open and stays in no fault list, and the
// installation mode no longer holds its tamper, to be reported when the mode closes.
// The other events of the registry (a device added or refused) are the installer's
// business.
func (h *Hub) registryMessage(m inbox.Message) {
	if _, err := inbox.Envelope(m, h.cfg.Site, contract.TypeRegistryDeviceRemoved); errors.Is(err, inbox.ErrWrongType) {
		return
	}
	_, d, ok := decode[contract.RegistryDeviceRemovedData](h, m, contract.TypeRegistryDeviceRemoved, "device removal")
	if !ok {
		return
	}
	if d.DeviceID == "" {
		h.log.Warn("ignoring a device removal without a device")
		return
	}
	err := h.alarm.loop.Submit(h.life, func(now time.Time) engine.Input {
		delete(h.alarm.inst.quieted, d.DeviceID)
		return engwire.Forget(d, now)
	})
	h.submitFailed(err)
	err = h.monitor.loop.Submit(h.life, func(now time.Time) health.Input { return health.Forget{Now: now, DeviceID: d.DeviceID} })
	h.submitFailed(err)
}

// --- the site's services -------------------------------------------------------------

// gsmStatusMessage takes the GSM gateway's lines: they go into system.health, and the
// monitor learns whether the SMS backup still works. The gateway repeats its status:
// the monitor notices when it stops (see health.GSM), and the gsm link follows the
// monitor.
func (h *Hub) gsmStatusMessage(m inbox.Message) {
	env, d, ok := decode[contract.GSMStatusData](h, m, contract.TypeGSMStatus, "GSM status")
	if !ok {
		return
	}
	if h.cfg.Health.WithoutGSM {
		// A gateway run against the site's configuration: its lines are not the
		// site's, and nothing asks it for an SMS.
		h.log.Debug("ignoring a GSM status: the site has no GSM line")
		return
	}
	h.gsm.Store(&contract.GSMHealth{Lines: d.Lines})
	in := healthwire.GSM(d, env.TS.Time)
	h.statusToMonitor(func(now time.Time) health.Input { in.Now = now; return in })
}

// zigbeeStatusMessage takes the Zigbee adapter's status: the monitor learns whether
// the sensors can be heard, and notices when the adapter stops repeating it.
func (h *Hub) zigbeeStatusMessage(m inbox.Message) {
	env, d, ok := decode[contract.ZigbeeStatusData](h, m, contract.TypeZigbeeStatus, "Zigbee status")
	if !ok {
		return
	}
	in := healthwire.Zigbee(d, env.TS.Time)
	h.statusToMonitor(func(now time.Time) health.Input { in.Now = now; return in })
}

// agentStatusMessage takes the link agent's status: the monitor learns whether the
// cloud can be reached, and notices when the agent stops repeating it. The internet
// link of system.health follows, and so does the escalation (see publishHealth).
func (h *Hub) agentStatusMessage(m inbox.Message) {
	env, d, ok := decode[contract.AgentStatusData](h, m, contract.TypeAgentStatus, "agent status")
	if !ok {
		return
	}
	in := healthwire.Internet(d, env.TS.Time)
	h.statusToMonitor(func(now time.Time) health.Input { in.Now = now; return in })
}

// statusToMonitor hands the monitor a service's status, then publishes system.health
// if it changed, even when the monitor's state did not move: the GSM lines may have.
func (h *Hub) statusToMonitor(build func(now time.Time) health.Input) {
	err := h.monitor.loop.Post(h.life, func(now time.Time) []health.Action {
		actions := h.monitor.mon.Handle(build(now))
		h.publishHealth(now)
		return actions
	})
	h.submitFailed(err)
}
