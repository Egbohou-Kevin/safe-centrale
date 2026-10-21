package main

import (
	"errors"
	"log"
	"strconv"
	"strings"
	"time"
)

var errUnknown = errors.New("unknown order or missing value")

// order applies one order of the pipe:
//
//	sms FROM TEXT...      an SMS arrives
//	network on|off        registered on the network, or not
//	signal N              the signal (0..31, 99 unknown)
//	sms-error on|off      every SMS fails (+CMS ERROR: 500)
//	silent on|off         the modem stops answering
//	after-dial LINE WAIT  the call progress after dialling (BUSY, NO CARRIER...)
//	unplug                the modem is pulled out and comes back
func (k *kit) order(f []string) error {
	switch {
	case f[0] == "sms":
		return k.incomingSMS(f[1:])
	case f[0] == "unplug":
		select {
		case k.unplug <- struct{}{}:
		default: // already on its way out
		}
		return nil
	case len(f) < 2:
		return errUnknown
	}
	k.sim.Mu.Lock()
	defer k.sim.Mu.Unlock()
	on := f[1] == "on"
	switch f[0] {
	case "network":
		k.sim.CREG = map[bool]string{true: "0,1", false: "0,2"}[on]
	case "signal":
		k.sim.CSQ = f[1] + ",99"
	case "sms-error":
		k.sim.CMGS = map[bool]string{true: "+CMS ERROR: 500", false: ""}[on]
	case "silent":
		k.sim.Silent = on
	case "after-dial":
		k.afterDial(f[1:])
	default:
		return errUnknown
	}
	log.Printf("order applied: %s", strings.Join(f, " "))
	return nil
}

// afterDial sets the line sent after a call is dialled, and when. k.sim.Mu is held.
func (k *kit) afterDial(f []string) {
	wait := 2 * time.Second
	if n := len(f); n > 1 {
		if d, err := time.ParseDuration(f[n-1]); err == nil {
			wait, f = d, f[:n-1]
		}
	}
	k.sim.AfterDial, k.sim.AfterWait = strings.Join(f, " "), wait
}

// incomingSMS stores an SMS and tells the gateway, as a modem does (+CMTI).
func (k *kit) incomingSMS(f []string) error {
	if len(f) < 2 {
		return errors.New("usage: sms FROM TEXT")
	}
	k.sim.Mu.Lock()
	k.inboxID++
	id := k.inboxID
	k.sim.Inbox[id] = [2]string{f[0], strings.Join(f[1:], " ")}
	k.sim.Mu.Unlock()
	k.sim.Push(`+CMTI: "SM",` + strconv.Itoa(id))
	log.Printf("SMS from %s stored at %d", f[0], id)
	return nil
}
