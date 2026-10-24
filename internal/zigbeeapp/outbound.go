package zigbeeapp

import (
	"context"
	"errors"
	"reflect"
	"time"

	"safehome/internal/contract"
	"safehome/internal/outbox"
	"safehome/internal/zigbee"
	"safehome/internal/zigbee/wire"
	"safehome/internal/zigbee/z2m"
)

// dispatch runs on the loop's goroutine and only queues messages. An action that
// fails does not stop the ones after it: an acknowledgement that cannot be built must
// not hold back the order to the siren.
func (a *App) dispatch(_ context.Context, now time.Time, actions []zigbee.Action) error {
	var errs []error
	for _, act := range actions {
		if err := a.dispatchOne(now, act); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (a *App) dispatchOne(now time.Time, act zigbee.Action) error {
	switch v := act.(type) {
	case zigbee.Changed:
		out, err := wire.Changed(v)
		if err != nil {
			a.log.Error("a value refused by the contract: check the profile", "device", v.DeviceID, "err", err)
			return nil
		}
		// A change is dated from when it happened: a key pressed while the adapter was
		// down keeps its age, and the hub lets it expire.
		ts := now
		if !v.Since.IsZero() && v.Since.Before(now) {
			ts = v.Since
		}
		a.publishDevice(ts, out)
	case zigbee.Presence:
		a.log.Info("device presence", "device", v.DeviceID, "online", v.Online)
		a.publishDevice(now, wire.Presence(v))
	case zigbee.Ack:
		return a.publishAck(now, v)
	case zigbee.Added, zigbee.Rejected, zigbee.Removed, zigbee.RegistryChanged, zigbee.PairFailed:
		return a.publishRegistryNews(now, act)
	case zigbee.Ignored:
		a.log.Debug("ignored", "what", v.What)
	default:
		msg, ok, err := z2m.Build(a.cfg.Base, act)
		if err != nil {
			return err
		}
		if ok {
			a.out.Put(&outbox.Item{Topic: msg.Topic, Data: msg.Payload, QoS: 1, Raw: true})
		}
	}
	return nil
}

func (a *App) publishAck(now time.Time, v zigbee.Ack) error {
	ack, err := wire.Ack(v)
	if err != nil {
		return err
	}
	if v.Status == zigbee.AckFailed || v.Status == zigbee.AckExpired {
		a.log.Warn("a device command did not succeed", "device", v.DeviceID, "status", v.Status, "reason", v.Reason)
	}
	a.out.Send(src, contract.DeviceAckTopic(a.cfg.Site, v.DeviceID), contract.TypeCommandAck, ack, false, now)
	return nil
}

// publishRegistryNews publishes what changed in the registry.
func (a *App) publishRegistryNews(now time.Time, act zigbee.Action) error {
	site := a.cfg.Site
	switch v := act.(type) {
	case zigbee.Added:
		a.log.Info("device added", "device", v.Device.ID, "profile", v.Device.Profile)
		a.out.Send(src, contract.RegistryEventTopic(site), contract.TypeRegistryDeviceAdded, wire.Added(v), false, now)
	case zigbee.Rejected:
		r, err := wire.Rejected(v)
		if err != nil {
			return err
		}
		a.log.Warn("device refused", "model", v.Model, "reason", v.Reason)
		a.out.Send(src, contract.RegistryEventTopic(site), contract.TypeRegistryDeviceRejected, r, false, now)
	case zigbee.PairFailed:
		a.log.Error("the network could not be opened for pairing", "reason", v.Reason)
		a.out.Send(src, contract.RegistryEventTopic(site), contract.TypeRegistryPairFailed, wire.PairFailed(v), false, now)
	case zigbee.Removed:
		a.log.Info("device removed", "device", v.Device.ID)
		a.out.Send(src, contract.RegistryEventTopic(site), contract.TypeRegistryDeviceRemoved, wire.Removed(v), false, now)
		for _, c := range wire.StateCapabilities(v.Device) {
			// An empty retained message clears the state: a removed door left
			// "open" would otherwise block every arming.
			a.out.Put(&outbox.Item{Topic: contract.DeviceStateTopic(site, v.Device.ID, c), QoS: 1, Retain: true, Raw: true})
		}
	case zigbee.RegistryChanged:
		a.publishRegistry(now, v.Devices)
	}
	return nil
}

func (a *App) publishDevice(now time.Time, d wire.Device) {
	if d.Event != nil {
		topic := contract.DeviceEventTopic(a.cfg.Site, d.Event.DeviceID)
		if d.Event.Code != "" {
			a.out.SendWithCode(src, topic, contract.TypeDeviceEvent, *d.Event, now) // kept by no broker
		} else {
			a.out.Send(src, topic, contract.TypeDeviceEvent, *d.Event, false, now)
		}
	}
	if d.State != nil {
		a.out.Send(src, contract.DeviceStateTopic(a.cfg.Site, d.State.DeviceID, d.State.Capability), contract.TypeDeviceState, *d.State, true, now)
	}
}

func (a *App) publishRegistry(now time.Time, devices []zigbee.Device) {
	a.out.Send(src, contract.RegistryDevicesTopic(a.cfg.Site), contract.TypeRegistrySnapshot, wire.Snapshot(devices), true, now)
}

// publishStatus publishes zigbee.status, retained, when it changed. It runs on the
// loop's goroutine.
func (a *App) publishStatus(now time.Time) {
	st := wire.Status(a.core.Status())
	if a.sent && reflect.DeepEqual(st, a.last) {
		return
	}
	a.last, a.sent = st, true
	a.out.Send(src, contract.ZigbeeStatusTopic(a.cfg.Site), contract.TypeZigbeeStatus, st, true, now)
}

// publishStatusAgain publishes zigbee.status even if it did not change.
func (a *App) publishStatusAgain(now time.Time) {
	a.sent = false
	a.publishStatus(now)
}
