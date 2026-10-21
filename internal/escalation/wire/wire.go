// Package wire translates between the escalation's domain types and the event
// contract. The escalation never imports the contract, so a new contract version
// only changes this package.
package wire

import (
	"fmt"
	"slices"
	"time"

	"safehome/internal/contract"
	"safehome/internal/escalation"
)

var kinds = map[escalation.AlertKind]string{
	escalation.KindIntrusion:      contract.AlertIntrusion,
	escalation.KindFire:           contract.AlertFire,
	escalation.KindPanic:          contract.AlertPanic,
	escalation.KindTamper:         contract.AlertTamper,
	escalation.KindAuthFailed:     contract.AlertAuthFailed,
	escalation.KindArmingBypassed: contract.AlertArmingBypassed,
	escalation.KindHubBatteryLow:  contract.AlertBatteryLowHub,
	escalation.KindMainsRestored:  contract.AlertMainsRestored,
	escalation.KindDeviceOffline:  contract.AlertDeviceOffline,
	escalation.KindBatteryLow:     contract.AlertBatteryLow,
	escalation.KindGSMDegraded:    contract.AlertGSMDegraded,
	escalation.KindZigbeeDown:     contract.AlertZigbeeDown,
}

// Kind returns the escalation kind of an alert kind of the contract: the site's
// configuration names the plans as the contract names the alerts.
func Kind(contractKind string) (escalation.AlertKind, bool) {
	for k, name := range kinds {
		if name == contractKind {
			return k, true
		}
	}
	return "", false
}

// KindNames lists the alert kinds of the contract that have a plan, sorted.
func KindNames() []string {
	out := make([]string, 0, len(kinds))
	for _, name := range kinds {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

var urgencies = map[escalation.Urgency]string{
	escalation.UrgencyCritical: contract.UrgencyCritical,
	escalation.UrgencyHigh:     contract.UrgencyHigh,
	escalation.UrgencyLow:      contract.UrgencyLow,
}

var channels = map[escalation.Channel]string{
	escalation.ChannelApp:      contract.ChannelApp,
	escalation.ChannelWhatsApp: contract.ChannelWhatsApp,
	escalation.ChannelSMS:      contract.ChannelSMS,
	escalation.ChannelCall:     contract.ChannelCall,
}

// Request turns a message to send into an alert.request payload, addressed to one
// contact through one channel.
func Request(s escalation.Send) (contract.AlertRequestData, error) {
	kind, ok := kinds[s.Kind]
	if !ok {
		return contract.AlertRequestData{}, fmt.Errorf("wire: unknown alert kind %q", s.Kind)
	}
	urgency, ok := urgencies[s.Urgency]
	if !ok {
		return contract.AlertRequestData{}, fmt.Errorf("wire: unknown urgency %q", s.Urgency)
	}
	channel, ok := channels[s.Channel]
	if !ok {
		return contract.AlertRequestData{}, fmt.Errorf("wire: unknown channel %q", s.Channel)
	}
	return contract.AlertRequestData{
		Incident: s.Incident, Kind: kind, Urgency: urgency, Percent: s.Percent, Devices: s.Devices,
		Wave: s.Wave, Contact: s.Contact, Channel: channel, Text: s.Text,
	}, nil
}

var statuses = map[string]escalation.ResultStatus{
	contract.ResultQueued:    escalation.ResultQueued,
	contract.ResultSent:      escalation.ResultSent,
	contract.ResultDelivered: escalation.ResultDelivered,
	contract.ResultFailed:    escalation.ResultFailed,
}

// Result turns an alert.result into an escalation input. key is the escalation's
// own number of the message the result is about: the caller found it from the
// result's ref. ok is false for a status the contract does not define.
func Result(d contract.AlertResultData, key uint64, now time.Time) (escalation.Result, bool) {
	status, ok := statuses[d.Status]
	if !ok {
		return escalation.Result{}, false
	}
	return escalation.Result{Now: now, Key: key, Status: status}, true
}

// ResultByRef turns an alert.result into an escalation input that finds its
// message by the request id (see escalation.Escalation.Bind), so the caller needs
// no index of its own. ok is false for a status the contract does not define.
func ResultByRef(d contract.AlertResultData, now time.Time) (escalation.Result, bool) {
	status, ok := statuses[d.Status]
	if !ok || d.Ref == "" {
		return escalation.Result{}, false
	}
	return escalation.Result{Now: now, Ref: d.Ref, Status: status}, true
}

// Ack turns an alert.ack into an escalation input.
func Ack(d contract.AlertAckData, now time.Time) escalation.Ack {
	return escalation.Ack{Now: now, Incident: d.Incident, Contact: d.Contact}
}
