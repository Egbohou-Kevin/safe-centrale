package contract

import (
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"slices"
)

// Live video (see docs/video.md). The cloud asks the site for a session, the site's
// video service pushes one camera's stream to the cloud relay given in the request,
// and reports how the session goes. Video never takes part in the alarm: if it
// fails, nothing else does.

// Why a session is asked for.
const (
	VideoForAlarm  = "alarm"  // offered by the app with an alarm notification
	VideoOnDemand  = "demand" // a user wants to look
	MaxVideoLength = 86400    // seconds: a bound against nonsense, not a product limit
)

// How the site reaches the internet. On a fixed line (the shop's box, by Wi-Fi or
// cable) watching costs nothing more: no allowance, no time limit. On mobile data
// (a 4G router) the data is paid by the client, who sets the allowance and the
// longest session.
const (
	UplinkFixed  = "fixed"
	UplinkMobile = "mobile"
)

// Statuses of a video session.
const (
	VideoStarting = "starting"
	VideoLive     = "live"
	VideoEnded    = "ended"
	VideoFailed   = "failed"
)

// Reasons for the end or the failure of a session.
const (
	VideoStopped           = "stopped"            // asked by the cloud (video.stop)
	VideoTimeout           = "timeout"            // max_s reached
	VideoQuota             = "quota"              // the site's monthly data allowance is used up
	VideoBusy              = "busy"               // too many sessions at once on the site
	VideoUnknownCamera     = "unknown_camera"     // no such camera on the site
	VideoCameraUnreachable = "camera_unreachable" // the camera does not answer
	VideoRelayUnreachable  = "relay_unreachable"  // the cloud relay cannot be reached
	VideoExpired           = "expired"            // the request arrived past its exp
)

var (
	videoReasons  = []string{VideoForAlarm, VideoOnDemand}
	videoUplinks  = []string{UplinkFixed, UplinkMobile}
	videoStatuses = []string{VideoStarting, VideoLive, VideoEnded, VideoFailed}
	videoEnds     = []string{VideoStopped, VideoTimeout, VideoQuota, VideoBusy, VideoUnknownCamera,
		VideoCameraUnreachable, VideoRelayUnreachable, VideoExpired}
)

// VideoRequestData: video.request, from the cloud on video/cmd. Token is the
// relay's one-session credential: like an access code it is never printed or
// logged (String, GoString and LogValue mask it).
type VideoRequestData struct {
	Session  string    `json:"session"`            // chosen by the cloud, repeated in every message of the session
	Camera   string    `json:"camera"`             // our camera id; the camera's address stays on the site
	Reason   string    `json:"reason"`             // alarm, demand
	User     string    `json:"user,omitempty"`     // who asked
	Incident string    `json:"incident,omitempty"` // the alarm, for reason alarm
	MaxS     int       `json:"max_s,omitempty"`    // the session stops by itself after this long; 0: no limit
	PushURL  string    `json:"push_url"`           // where to send the stream: rtmps, no query
	Token    string    `json:"token"`              // credential for push_url, this session only: sent as ?token=
	Exp      Timestamp `json:"exp"`                // a request seen after exp is refused
}

func (d VideoRequestData) redacted() VideoRequestData {
	if d.Token != "" {
		d.Token = "***"
	}
	return d
}

// String masks the token.
func (d VideoRequestData) String() string {
	r := d.redacted()
	return fmt.Sprintf("{session:%q camera:%q reason:%q user:%q max_s:%d push_url:%q token:%q}",
		r.Session, r.Camera, r.Reason, r.User, r.MaxS, r.PushURL, r.Token)
}

// GoString masks the token for %#v.
func (d VideoRequestData) GoString() string { return d.String() }

// LogValue masks the token in slog logs.
func (d VideoRequestData) LogValue() slog.Value {
	r := d.redacted()
	return slog.GroupValue(
		slog.String("session", r.Session), slog.String("camera", r.Camera), slog.String("reason", r.Reason),
		slog.String("user", r.User), slog.Int("max_s", r.MaxS), slog.String("push_url", r.PushURL),
		slog.String("token", r.Token),
	)
}

// Validate checks the request. The token is never part of an error.
func (d VideoRequestData) Validate() error {
	switch {
	case d.Session == "" || d.Camera == "":
		return fmt.Errorf("%w: session and camera are required", ErrInvalid)
	case !slices.Contains(videoReasons, d.Reason):
		return fmt.Errorf("%w: unknown video reason %q", ErrInvalid, d.Reason)
	case d.MaxS < 0 || d.MaxS > MaxVideoLength:
		return fmt.Errorf("%w: max_s must be from 0 (no limit) to %d", ErrInvalid, MaxVideoLength)
	case d.Token == "":
		return fmt.Errorf("%w: a token is required", ErrInvalid)
	case d.Exp.IsZero():
		return fmt.Errorf("%w: exp is required", ErrInvalid)
	}
	u, err := url.Parse(d.PushURL)
	if err != nil || u.Scheme != "rtmps" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: push_url must be an rtmps address without credentials nor query", ErrInvalid)
	}
	// go2rtc checks no certificate when the host is an IP address: anyone on the way
	// (the operator, the shop's box, a spoofer on its network) would read the token,
	// which the connection carries, and the shop's video. Only a name is checked.
	if _, err := netip.ParseAddr(u.Hostname()); err == nil {
		return fmt.Errorf("%w: push_url must name its host, not give its address", ErrInvalid)
	}
	return nil
}

// VideoStopData: video.stop, from the cloud on video/cmd.
type VideoStopData struct {
	Session string `json:"session"`
}

// Validate checks the order.
func (d VideoStopData) Validate() error {
	if d.Session == "" {
		return fmt.Errorf("%w: empty session", ErrInvalid)
	}
	return nil
}

// VideoSessionData: video.session, from the site on video/session, at each step
// of a session.
type VideoSessionData struct {
	Session   string `json:"session"`
	Camera    string `json:"camera"`
	Status    string `json:"status"`               // starting, live, ended, failed
	Reason    string `json:"reason,omitempty"`     // for ended and failed
	DurationS int    `json:"duration_s,omitempty"` // for ended: how long it was live
	Bytes     int64  `json:"bytes,omitempty"`      // for ended: data sent, for the allowance
}

// VideoCamera is a camera as the site's video service sees it.
type VideoCamera struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"` // "Caisse", "Arriere-cour"
	Reachable bool   `json:"reachable"`
}

// VideoStatusData: video.status, retained on video/status: the cameras, the
// sessions running, how the site reaches the internet, and on mobile data the
// month's allowance set by the client.
type VideoStatusData struct {
	Cameras     []VideoCamera `json:"cameras"`
	Active      int           `json:"active"`        // sessions running
	Uplink      string        `json:"uplink"`        // fixed, mobile
	UsedMBMonth int           `json:"used_mb_month"` // data sent this month
	QuotaMB     int           `json:"quota_mb"`      // allowance per month, 0 for none (always 0 on a fixed line)
	MaxS        int           `json:"max_s"`         // longest session set by the client, 0 for none (always 0 on a fixed line)
}
