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
//	pair MODEL IEEE     a device joins and its interview succeeds
//	send IEEE JSON      the device reports (last_seen is added)
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
		z.event("device_interview", f[2], f[1])
	case "send":
		return z.report(f[1], strings.Join(f[2:], " "))
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

// report publishes what a device says, on its current name.
func (z *bridge) report(ieee, raw string) error {
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return err
	}
	fields["last_seen"] = time.Now().UTC().Format(time.RFC3339)
	z.mu.Lock()
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
