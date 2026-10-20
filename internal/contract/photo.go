package contract

import (
	"fmt"
	"log/slog"
	"net/url"
	"slices"
)

// Photos at an alarm (see docs/video.md). The video service takes a few reduced
// pictures from the cameras when the alarm goes off and announces each one
// (photo.taken). The cloud answers with where to send it (photo.upload); the link
// agent sends the file over HTTPS, after the alarm's messages, never before them, and
// says how it went (photo.sent). The pictures never travel over MQTT.

// PhotoMaxAge is how long a photo waits on the site for its upload: past it, it is
// dropped (photo.sent, failed, too_old).
const PhotoMaxAge = 24 * 60 * 60 // seconds

// Statuses and reasons of photo.sent.
const (
	PhotoSent   = "sent"
	PhotoFailed = "failed"

	PhotoURLExpired = "url_expired" // the address came too late or was refused: the cloud may send a new photo.upload
	PhotoMissing    = "missing"     // the file is no longer on the site
	PhotoTooOld     = "too_old"     // it waited longer than PhotoMaxAge
	PhotoRefused    = "refused"     // the cloud refused the file itself
)

var (
	photoStatuses = []string{PhotoSent, PhotoFailed}
	photoFailures = []string{PhotoURLExpired, PhotoMissing, PhotoTooOld, PhotoRefused}
)

// PhotoTakenData: photo.taken, from the video service on photo/taken. The envelope's
// ts is when the picture was taken.
type PhotoTakenData struct {
	Photo    string `json:"photo"`    // ULID, chosen by the site
	Incident string `json:"incident"` // the alarm
	Camera   string `json:"camera"`
	Bytes    int64  `json:"bytes"` // the JPEG's size
	Width    int    `json:"width"` // in pixels
}

// Validate checks the announcement.
func (d PhotoTakenData) Validate() error {
	switch {
	case d.Photo == "" || d.Incident == "" || d.Camera == "":
		return fmt.Errorf("%w: photo, incident and camera are required", ErrInvalid)
	case d.Bytes <= 0 || d.Width <= 0:
		return fmt.Errorf("%w: bytes and width must be positive", ErrInvalid)
	}
	return nil
}

// PhotoUploadData: photo.upload, from the cloud on photo/cmd, for the link agent:
// where to send a photo. Token is a credential for this photo only, sent as
// "Authorization: Bearer": like an access code it is never printed or logged.
type PhotoUploadData struct {
	Photo string    `json:"photo"`
	URL   string    `json:"url"`   // https, without credentials nor query
	Token string    `json:"token"` // for url, this photo only
	Exp   Timestamp `json:"exp"`   // the address is no good after exp
}

func (d PhotoUploadData) redacted() PhotoUploadData {
	if d.Token != "" {
		d.Token = "***"
	}
	return d
}

// String masks the token.
func (d PhotoUploadData) String() string {
	r := d.redacted()
	return fmt.Sprintf("{photo:%q url:%q token:%q exp:%s}", r.Photo, r.URL, r.Token, r.Exp.UTC().Format("2006-01-02T15:04:05Z"))
}

// GoString masks the token for %#v.
func (d PhotoUploadData) GoString() string { return d.String() }

// LogValue masks the token in slog logs.
func (d PhotoUploadData) LogValue() slog.Value {
	r := d.redacted()
	return slog.GroupValue(slog.String("photo", r.Photo), slog.String("url", r.URL), slog.String("token", r.Token))
}

// Validate checks the order. The token is never part of an error.
func (d PhotoUploadData) Validate() error {
	switch {
	case d.Photo == "":
		return fmt.Errorf("%w: photo is required", ErrInvalid)
	case d.Token == "":
		return fmt.Errorf("%w: a token is required", ErrInvalid)
	case d.Exp.IsZero():
		return fmt.Errorf("%w: exp is required", ErrInvalid)
	}
	u, err := url.Parse(d.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: url must be an https address without credentials nor query", ErrInvalid)
	}
	return nil
}

// PhotoSentData: photo.sent, from the link agent on photo/sent.
type PhotoSentData struct {
	Photo  string `json:"photo"`
	Status string `json:"status"`           // sent, failed
	Reason string `json:"reason,omitempty"` // for failed
}

// Validate checks the report.
func (d PhotoSentData) Validate() error {
	switch {
	case d.Photo == "":
		return fmt.Errorf("%w: photo is required", ErrInvalid)
	case !slices.Contains(photoStatuses, d.Status):
		return fmt.Errorf("%w: unknown photo status %q", ErrInvalid, d.Status)
	case d.Status == PhotoFailed && !slices.Contains(photoFailures, d.Reason):
		return fmt.Errorf("%w: unknown photo failure %q", ErrInvalid, d.Reason)
	case d.Status == PhotoSent && d.Reason != "":
		return fmt.Errorf("%w: a sent photo has no reason", ErrInvalid)
	}
	return nil
}
