package zigbeeapp

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"safehome/internal/contract"
	"safehome/internal/zigbee"
	"safehome/internal/zigbee/wire"
	"safehome/internal/zigbee/z2m"
)

// HandleMessage takes a message from the broker. It returns at once: the work is
// queued on the core's loop.
func (a *App) HandleMessage(m Message) {
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
func (a *App) fromBridge(m Message) {
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

// envelope parses and checks the envelope of an order, and drops a duplicate.
func (a *App) envelope(m Message, types ...string) (contract.Envelope, bool) {
	env, err := contract.ParseEnvelope(m.Payload)
	if err != nil || env.Site != a.cfg.Site {
		a.log.Warn("ignoring an invalid order", "topic", m.Topic, "err", err)
		return contract.Envelope{}, false
	}
	if !slices.Contains(types, env.Type) {
		a.log.Warn("ignoring a message of the wrong type for its topic", "topic", m.Topic, "type", env.Type)
		return contract.Envelope{}, false
	}
	return env, a.seen.FirstTime(env.ID)
}

// decodeOrder reads the data of an order and checks it against the contract.
func decodeOrder[T interface{ Validate() error }](env contract.Envelope) (T, error) {
	d, err := contract.DecodeData[T](env)
	if err != nil {
		return d, err
	}
	return d, d.Validate()
}

func (a *App) deviceCommand(deviceID string, m Message) {
	env, ok := a.envelope(m, contract.TypeCommand)
	if !ok {
		return
	}
	d, err := decodeOrder[contract.DeviceCommandData](env)
	if err == nil && d.DeviceID != deviceID {
		err = fmt.Errorf("topic for %s, command for %s", deviceID, d.DeviceID)
	}
	if err != nil {
		a.log.Warn("ignoring an invalid device command", "topic", m.Topic, "err", err)
		return
	}
	a.submit(func(now time.Time) zigbee.Input { return wire.Command(d, env.ID, now) })
}

func (a *App) registryOrder(m Message) {
	env, ok := a.envelope(m, contract.TypeRegistryPair, contract.TypeRegistryRemove)
	if !ok {
		return
	}
	if env.Type == contract.TypeRegistryPair {
		d, err := decodeOrder[contract.RegistryPairData](env)
		if err != nil {
			a.log.Warn("ignoring an invalid pairing order", "err", err)
			return
		}
		a.log.Info("pairing order", "seconds", d.DurationS)
		a.submit(func(now time.Time) zigbee.Input { return wire.Pair(d, env.ID, now) })
		return
	}
	d, err := decodeOrder[contract.RegistryRemoveData](env)
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
