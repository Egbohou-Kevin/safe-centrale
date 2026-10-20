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

// Request turns an alert.request into a gateway input. ok is false when the request
// is not for the GSM channels (app, WhatsApp) or has no contact.
func Request(d contract.AlertRequestData, id string, now time.Time) (gsm.Request, bool) {
	ch, ok := channels[d.Channel]
	if !ok || d.Contact == "" || id == "" {
		return gsm.Request{}, false
	}
	return gsm.Request{
		Now: now, ID: id, Incident: d.Incident, Contact: d.Contact, Channel: ch,
		Priority: priority(d.Kind), Text: d.Text,
	}, true
}

// priority: what protects lives first, then intrusions, then the rest.
func priority(kind string) gsm.Priority {
	switch kind {
	case contract.AlertFire, contract.AlertPanic:
		return gsm.PriorityLifeSafety
	case contract.AlertIntrusion:
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

// Status turns the lines into a gsm.status payload.
func Status(lines []gsm.LineStatus, queued int) contract.GSMStatusData {
	out := contract.GSMStatusData{Lines: make([]contract.GSMLine, 0, len(lines)), Queued: queued}
	for _, l := range lines {
		cl := contract.GSMLine{
			ID: l.ID, Operator: l.Operator, Responding: l.Responding, Registered: l.Registered,
			Signal: l.Signal, Credit: l.Credit, LastTestOK: l.LastTestOK, Restarts: l.Restarts,
		}
		if !l.LastTest.IsZero() {
			cl.LastTest = contract.TSPtr(l.LastTest)
		}
		out.Lines = append(out.Lines, cl)
	}
	return out
}
