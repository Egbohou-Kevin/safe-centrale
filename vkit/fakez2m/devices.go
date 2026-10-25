package main

import (
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"
)

// order applies one order of the pipe:
//
//	pair MODEL IEEE     a device joins, and its interview succeeds
//	send IEEE JSON      the device reports (last_seen is added)
//	say IEEE JSON       Zigbee2MQTT publishes by itself, with the device's last last_seen:
//	                    no frame came from it (the motion sensor's "no movement", on
//	                    Zigbee2MQTT's own timer)
//	leave IEEE          the device leaves the network
//	bridge online|offline
//	answers on|off      off: requests go unanswered (a Zigbee2MQTT stuck)
func (z *bridge) order(f []string) error {
	if len(f) < 2 {
		return errors.New("missing value")
	}
	switch f[0] {
	case "pair":
		if len(f) != 3 {
			return errors.New("usage: pair MODEL IEEE")
		}
		z.event("device_joined", f[2], "")
		z.event("device_interview", f[2], f[1])
	case "send":
		return z.report(f[1], strings.Join(f[2:], " "), false)
	case "say":
		return z.report(f[1], strings.Join(f[2:], " "), true)
	case "leave":
		z.event("device_leave", f[1], "")
	case "bridge":
		z.publish("bridge/state", map[string]string{"state": f[1]}, true)
	case "answers":
		z.mu.Lock()
		z.answers = f[1] != "off"
		z.mu.Unlock()
	default:
		return errors.New("unknown order")
	}
	return nil
}

// event publishes a bridge event about a device.
func (z *bridge) event(kind, ieee, model string) {
	data := map[string]any{"ieee_address": ieee, "friendly_name": ieee}
	if kind == "device_interview" {
		data["status"], data["supported"] = "successful", true
		data["definition"] = map[string]string{"model": model}
	}
	z.publish("bridge/event", map[string]any{"type": kind, "data": data}, false)
}

// report publishes what a device says, on its current name. Zigbee2MQTT stamps every
// message with when the device was last heard, to the millisecond: a new time for a
// frame from the device, the last one for what Zigbee2MQTT says by itself (own).
func (z *bridge) report(ieee, raw string, own bool) error {
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return err
	}
	z.mu.Lock()
	if z.seen == nil {
		z.seen = map[string]string{}
	}
	if _, heard := z.seen[ieee]; !own || !heard {
		z.seen[ieee] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	fields["last_seen"] = z.seen[ieee]
	name, ok := z.names[ieee]
	qos, set := z.qos[ieee]
	z.mu.Unlock()
	if !ok {
		name = ieee // not renamed (yet): Zigbee2MQTT uses the address
	}
	if !set {
		qos = 1 // device_options
	}
	log.Printf("REPORT %s qos %d", name, qos)
	z.publishQoS(name, fields, qos, false)
	return nil
}
