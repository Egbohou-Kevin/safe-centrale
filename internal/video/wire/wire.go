// Package wire translates between the video core's types and the event contract.
// The core never imports the contract, so a new contract version only changes this
// package.
package wire

import (
	"net/url"
	"time"

	"safehome/internal/contract"
	"safehome/internal/video"
)

// mb is the megabyte of quota_mb and used_mb_month.
const mb = 1 << 20

// Request turns a video.request into a core input. The target is push_url with the
// session's token added as ?token=: the core passes it on without keeping it.
func Request(d contract.VideoRequestData, now time.Time) video.Request {
	return video.Request{
		Now: now, Session: d.Session, Camera: d.Camera,
		MaxLength: time.Duration(d.MaxS) * time.Second, Exp: d.Exp.Time,
		Target: d.PushURL + "?token=" + url.QueryEscape(d.Token),
	}
}

// Alarm turns an alarm.triggered, written at ts, into a core input: time for photos.
func Alarm(d contract.TriggeredData, ts, now time.Time) video.Alarm {
	return video.Alarm{Now: now, At: ts, Incident: d.Incident}
}

// Stop turns a video.stop into a core input.
func Stop(d contract.VideoStopData, now time.Time) video.Stop {
	return video.Stop{Now: now, Session: d.Session}
}

// Session turns a report into a video.session.
func Session(r video.Report) contract.VideoSessionData {
	return contract.VideoSessionData{
		Session: r.Session, Camera: r.Camera, Status: string(r.Status), Reason: string(r.Reason),
		DurationS: int(r.Duration / time.Second), Bytes: r.Bytes,
	}
}

// Status turns the core's state into a video.status.
func Status(st video.State) contract.VideoStatusData {
	d := contract.VideoStatusData{
		Cameras: []contract.VideoCamera{}, Active: st.Active, Uplink: contract.UplinkFixed,
		UsedMBMonth: int(st.Used / mb), QuotaMB: int(st.Quota / mb), MaxS: int(st.MaxLength / time.Second),
	}
	if st.Mobile {
		d.Uplink = contract.UplinkMobile
	}
	for _, c := range st.Cameras {
		d.Cameras = append(d.Cameras, contract.VideoCamera{ID: c.ID, Name: c.Name, Reachable: c.Reachable})
	}
	return d
}
