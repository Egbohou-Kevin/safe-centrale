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

// HandleMessage takes a message from the broker, and says whether it took it (see
// mqtt.Options.Take). It returns at once: the work is queued on the core's loop. Only
// a message that reaches it once the adapter is stopping is not taken: the broker
// keeps it for the next start, a door opened during a restart is not lost. Every
// other message is taken, read or dropped.
func (a *App) HandleMessage(m inbox.Message) bool {
	if len(m.Payload) > maxPayload {
		a.log.Warn("ignoring an oversized message", "topic", m.Topic, "bytes", len(m.Payload))
		return true
	}
	if strings.HasPrefix(m.Topic, a.cfg.Base+"/") {
		return a.fromBridge(m)
	}
	site, parts, ok := contract.ParseTopic(m.Topic)
	switch {
	case !ok || site != a.cfg.Site:
		return true
	case len(parts) == 3 && parts[0] == "device" && parts[2] == "cmd":
		return a.deviceCommand(parts[1], m)
	case len(parts) == 2 && parts[0] == "registry" && parts[1] == "cmd":
		return a.registryOrder(m)
	}
	return true
}

// fromBridge reads a message from Zigbee2MQTT. It is parsed here first, to log what
// cannot be read without queuing it, then again on the loop with the time of the
// moment.
func (a *App) fromBridge(m inbox.Message) bool {
	// A refusal is logged whatever the request; the core reads those it acts on (a
	// network that would not open).
	if req, reason, failed := z2m.ResponseError(a.cfg.Base, m.Topic, m.Payload); failed {
		a.log.Error("Zigbee2MQTT refused a request", "request", req, "err", reason)
	}
	if _, ok, err := z2m.Parse(a.cfg.Base, m.Topic, m.Payload, m.Retain, time.Time{}); err != nil || !ok {
		if err != nil {
			a.log.Warn("ignoring a message from Zigbee2MQTT", "topic", m.Topic, "err", err)
		}
		return true
	}
	return a.submit(func(now time.Time) zigbee.Input {
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

func (a *App) deviceCommand(deviceID string, m inbox.Message) bool {
	env, d, ok := order[contract.DeviceCommandData](a, m, contract.TypeCommand, "device command")
	if !ok {
		return true
	}
	if d.DeviceID != deviceID {
		a.log.Warn("ignoring a device command whose topic and content disagree", "topic", m.Topic, "device", d.DeviceID)
		return true
	}
	return a.submit(func(now time.Time) zigbee.Input { return wire.Command(d, env.ID, now) })
}

// orderMaxAge is how long a registry order stays good after it was written. The
// broker keeps the adapter's session for a day, and the installer's tool gives up
// after some seconds without an answer: a pairing order kept while the adapter was
// down must not open the network hours later, for nobody.
const orderMaxAge = time.Minute

func (a *App) registryOrder(m inbox.Message) bool {
	env, err := inbox.Envelope(m, a.cfg.Site, contract.TypeRegistryPair, contract.TypeRegistryRemove)
	if err != nil {
		a.log.Warn("ignoring an invalid registry order", "topic", m.Topic, "err", err)
		return true
	}
	if !a.seen.FirstTime(env.ID) {
		return true
	}
	if age := a.deps.Clock.Now().Sub(env.TS.Time); age > orderMaxAge {
		a.log.Warn("ignoring a registry order written too long ago: the tool that sent it gave up",
			"type", env.Type, "age", age.Round(time.Second))
		return true
	}
	if env.Type == contract.TypeRegistryPair {
		d, err := contract.ValidData[contract.RegistryPairData](env)
		if err != nil {
			a.log.Warn("ignoring an invalid pairing order", "err", err)
			return true
		}
		a.log.Info("pairing order", "seconds", d.DurationS)
		return a.submit(func(now time.Time) zigbee.Input { return wire.Pair(d, env.ID, now) })
	}
	d, err := contract.ValidData[contract.RegistryRemoveData](env)
	if err != nil {
		a.log.Warn("ignoring an invalid removal order", "err", err)
		return true
	}
	a.log.Info("removal order", "device", d.DeviceID)
	return a.submit(func(now time.Time) zigbee.Input { return wire.Remove(d, env.ID, now) })
}

// submit hands an input to the core, and says whether the loop took it: a loop that
// took an input carries it out, even when it stops. Once the adapter is stopping,
// nothing is taken.
func (a *App) submit(build func(time.Time) zigbee.Input) bool {
	err := a.loop.Submit(a.life, build)
	if err != nil && a.life.Err() == nil {
		a.log.Error("handing an input to the core failed", "err", err)
	}
	return err == nil || a.life.Err() == nil
}
