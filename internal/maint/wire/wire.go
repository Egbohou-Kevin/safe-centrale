// Package wire translates between the maintenance core's types and the event
// contract.
package wire

import (
	"time"

	"safehome/internal/contract"
	"safehome/internal/maint"
)

// Open turns a maint.open into a core input.
func Open(d contract.MaintOpenData, now time.Time) maint.Open {
	return maint.Open{Now: now, Session: d.Session, User: d.User, Length: time.Duration(d.DurationS) * time.Second, Exp: d.Exp.Time}
}

// Close turns a maint.close into a core input.
func Close(d contract.MaintCloseData, now time.Time) maint.Close {
	return maint.Close{Now: now, Session: d.Session}
}

// Session turns a report into a maint.session.
func Session(r maint.Report) contract.MaintSessionData {
	d := contract.MaintSessionData{
		Session: r.Session, User: r.User, Status: string(r.Status), Reason: string(r.Reason), DurationS: int(r.Duration / time.Second),
	}
	if !r.Until.IsZero() {
		d.Until = contract.TSPtr(r.Until)
	}
	return d
}
