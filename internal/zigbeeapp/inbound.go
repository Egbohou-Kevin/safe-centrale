package zigbeeapp

import (
	"strings"
	"time"

	"safehome/internal/contract"
	"safehome/internal/inbox"
	"safehome/internal/zigbee"
	"safehome/internal/zigbee/wire"
	"safehome/internal/zigbee/z2m"
)

// HandleMessage takes a message from the broker. It returns at once: the work is
// queued on the core's loop.
func (a *App) HandleMessage(m inbox.Message) {
	if len(m.Payload) > maxPayload {
		a.log.Warn("ignoring an oversized message", "topic", m.Topic, "bytes", len(m.Payload))
		return
	}
	if strings.HasPrefix(m.Topic, a.cfg.Base+"/") {
		a.fromBridge(m)
		return
	}
	site, parts, ok := contract.ParseTopic(m.Topic)
	if !ok || site != a.cfg.Site {
		return
	}
	switch {
	case len(parts) == 3 && parts[0] == "device" && parts[2] == "cmd":
		a.deviceCommand(parts[1], m)
	case len(parts) == 2 && parts[0] == "registry" && parts[1] == "cmd":
		a.registryOrder(m)
	}
}

// fromBridge reads a message from Zigbee2MQTT. It is parsed here first, to log what
// cannot be read without queuing it, then again on the loop with the time of the
// moment.
func (a *App) fromBridge(m inbox.Message) {
	if req, reason, failed := z2m.ResponseError(a.cfg.Base, m.Topic, m.Payload); failed {
		a.log.Error("Zigbee2MQTT refused a request", "request", req, "error", reason)
		return
	}
	if _, ok, err := z2m.Parse(a.cfg.Base, m.Topic, m.Payload, m.Retain, time.Time{}); err != nil || !ok {
		if err != nil {
			a.log.Warn("ignoring a message from Zigbee2MQTT", "topic", m.Topic, "err", err)
		}
		return
	}
	a.submit(func(now time.Time) zigbee.Input {
		in, _, _ := z2m.Parse(a.cfg.Base, m.Topic, m.Payload, m.Retain, now)
		return in
	})
}

// order reads an order through the inbox's gate, and drops a duplicate.
func order[T any](a *App, m inbox.Message, typ, what string) (contract.Envelope, T, bool) {
	env, d, err := inbox.Open[T](m, a.cfg.Site, typ)
	if err != nil {
		a.log.Warn("ignoring an invalid "+what, "topic", m.Topic, "err", err)
		return env, d, false
	}
	return env, d, a.seen.FirstTime(env.ID)
}

func (a *App) deviceCommand(deviceID string, m inbox.Message) {
	env, d, ok := order[contract.DeviceCommandData](a, m, contract.TypeCommand, "device command")
	if !ok {
		return
	}
	if d.DeviceID != deviceID {
		a.log.Warn("ignoring a device command whose topic and content disagree", "topic", m.Topic, "device", d.DeviceID)
		return
	}
	a.submit(func(now time.Time) zigbee.Input { return wire.Command(d, env.ID, now) })
}

func (a *App) registryOrder(m inbox.Message) {
	env, err := inbox.Envelope(m, a.cfg.Site, contract.TypeRegistryPair, contract.TypeRegistryRemove)
	if err != nil {
		a.log.Warn("ignoring an invalid registry order", "topic", m.Topic, "err", err)
		return
	}
	if !a.seen.FirstTime(env.ID) {
		return
	}
	if env.Type == contract.TypeRegistryPair {
		d, err := contract.ValidData[contract.RegistryPairData](env)
		if err != nil {
			a.log.Warn("ignoring an invalid pairing order", "err", err)
			return
		}
		a.log.Info("pairing order", "seconds", d.DurationS)
		a.submit(func(now time.Time) zigbee.Input { return wire.Pair(d, env.ID, now) })
		return
	}
	d, err := contract.ValidData[contract.RegistryRemoveData](env)
	if err != nil {
		a.log.Warn("ignoring an invalid removal order", "err", err)
		return
	}
	a.log.Info("removal order", "device", d.DeviceID)
	a.submit(func(now time.Time) zigbee.Input { return wire.Remove(d, env.ID, now) })
}

func (a *App) submit(build func(time.Time) zigbee.Input) {
	if err := a.loop.Submit(a.life, build); err != nil && a.life.Err() == nil {
		a.log.Error("handing an input to the core failed", "err", err)
	}
}
