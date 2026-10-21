// Package wire translates between the GSM gateway's domain types and the event
// contract. The gateway never imports the contract, so a new contract version only
// changes this package.
package wire

import (
	"time"

	"safehome/internal/contract"
	"safehome/internal/gsm"
)

var channels = map[string]gsm.Channel{
	contract.ChannelSMS:  gsm.ChannelSMS,
	contract.ChannelCall: gsm.ChannelCall,
}

// Request turns an alert.request into a gateway input, without its Now. id and ts
// come from the envelope: ts is when the escalation asked. ok is false when the
// request is not for the GSM channels (app, WhatsApp) or has no contact.
func Request(d contract.AlertRequestData, id string, ts time.Time) (gsm.Request, bool) {
	ch, ok := channels[d.Channel]
	if !ok || d.Contact == "" || id == "" {
		return gsm.Request{}, false
	}
	return gsm.Request{
		Asked: ts, ID: id, Incident: d.Incident, Contact: d.Contact, Channel: ch,
		Priority: priority(d.Kind), Text: d.Text,
	}, true
}

// Cleared turns an alarm.cleared into a gateway input, without its Now: the
// messages of an alarm stopped by a user have no reason to go. An alarm whose
// siren cycles ran out (origin timeout) is not over for the escalation, which goes
// on: ok is false.
func Cleared(d contract.ClearedData) (gsm.Cancel, bool) {
	return gsm.Cancel{Incident: d.Incident}, d.Origin == contract.OriginUser && d.Incident != ""
}

// Acked turns an alert.ack into a gateway input, without its Now: a contact took
// the incident in hand and its escalation is over.
func Acked(d contract.AlertAckData) (gsm.Cancel, bool) {
	return gsm.Cancel{Incident: d.Incident}, d.Incident != ""
}

// priority: what protects lives first, then intrusions, then the rest.
func priority(kind string) gsm.Priority {
	switch kind {
	case contract.AlertFire, contract.AlertPanic:
		return gsm.PriorityLifeSafety
	case contract.AlertIntrusion, contract.AlertSirenFailed:
		// A silent siren belongs to the alarm going on: it leaves with the intrusion,
		// not behind the daily reports, and is stale as soon as the alarm is.
		return gsm.PriorityIntrusion
	}
	return gsm.PriorityOther
}

var statuses = map[gsm.Status]string{
	gsm.StatusQueued:    contract.ResultQueued,
	gsm.StatusSent:      contract.ResultSent,
	gsm.StatusDelivered: contract.ResultDelivered,
	gsm.StatusFailed:    contract.ResultFailed,
}

// Result turns a report into an alert.result payload.
func Result(r gsm.Report) contract.AlertResultData {
	return contract.AlertResultData{Ref: r.Ref, Status: statuses[r.Status], Reason: string(r.Reason)}
}

// Ack turns a contact's "1" into an alert.ack payload.
func Ack(a gsm.Acknowledge) contract.AlertAckData {
	return contract.AlertAckData{Incident: a.Incident, Contact: a.Contact, Via: contract.ChannelSMS}
}

// credits translates a line's credit; an unknown one is absent.
var credits = map[gsm.Credit]string{gsm.CreditOK: contract.CreditOK, gsm.CreditEmpty: contract.CreditEmpty}

// Status turns the lines into a gsm.status payload.
func Status(lines []gsm.LineStatus, queued int) contract.GSMStatusData {
	out := contract.GSMStatusData{Lines: make([]contract.GSMLine, 0, len(lines)), Queued: queued}
	for _, l := range lines {
		cl := contract.GSMLine{
			ID: l.ID, Operator: l.Operator, Responding: l.Responding, Registered: l.Registered,
			Signal: l.Signal, Credit: credits[l.Credit], LastTestOK: l.LastTestOK, Restarts: l.Restarts,
		}
		if !l.LastTest.IsZero() {
			cl.LastTest = contract.TSPtr(l.LastTest)
		}
		out.Lines = append(out.Lines, cl)
	}
	return out
}
