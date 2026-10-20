// Package videoconfig reads the video service's configuration file (JSON), strictly:
// a misspelt key is an error. The cameras' addresses, passwords included, are in it:
// the file is readable by the service only (see docs/systeme.md), and nothing here
// prints them. See config/video.example.json.
package videoconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"safehome/internal/siteconfig"
	"safehome/internal/video"
	"safehome/internal/videoapp"
)

// Result is what the file describes.
type Result struct {
	App       videoapp.Config
	Sources   map[string]string // camera id -> its RTSP address; never printed
	Go2rtc    string            // the go2rtc executable
	FFmpeg    string            // decodes the photos' pictures
	PhotoDir  string            // where the photos wait for the link agent
	BrokerURL string
	ClientID  string
	StateDir  string
	LogLevel  slog.Level

	// SiteVersion is the version of the configuration set from the cloud found in
	// the state folder (0: none); SiteErr says why it was not applied, SiteNote what
	// was left out (the video settings, on a fixed line).
	SiteVersion int
	SiteErr     error
	SiteNote    string
}

// String hides the cameras' addresses.
func (r Result) String() string {
	return fmt.Sprintf("{site:%s cameras:%d go2rtc:%s broker:%s state:%s}", r.App.Site, len(r.Sources), r.Go2rtc, r.BrokerURL, r.StateDir)
}

// GoString hides them for %#v.
func (r Result) GoString() string { return r.String() }

type file struct {
	Site         string       `json:"site"`
	BrokerURL    string       `json:"broker_url"`
	ClientID     string       `json:"client_id"`
	StateDir     string       `json:"state_dir"`
	LogLevel     string       `json:"log_level"`
	Go2rtc       string       `json:"go2rtc"`
	FFmpeg       string       `json:"ffmpeg"`
	Photos       *bool        `json:"photos"`
	Uplink       string       `json:"uplink"`
	QuotaMB      *int         `json:"quota_mb"`
	MaxSession   *string      `json:"max_session"`
	MaxSessions  *int         `json:"max_sessions"`
	StartTimeout string       `json:"start_timeout"`
	StallAfter   string       `json:"stall_after"`
	ProbeEvery   string       `json:"probe_every"`
	Cameras      []cameraFile `json:"cameras"`
}

type cameraFile struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Source string `json:"source"`
}

var cameraID = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Load reads the configuration at path.
func Load(path string) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("videoconfig: %w", err)
	}
	r, err := parse(data)
	if err != nil {
		return Result{}, fmt.Errorf("videoconfig: %s: %w", path, err)
	}
	return withSite(r), nil
}

// withSite takes the client's allowance and longest session from the configuration
// set from the cloud, if the state folder holds one (internal/siteconfig). They only
// apply on mobile data.
func withSite(r Result) Result {
	o, ok, err := siteconfig.Read(r.StateDir)
	if err != nil || !ok {
		r.SiteErr = err
		return r
	}
	r.SiteVersion = o.Version
	if o.Video == nil {
		return r
	}
	if !r.App.Video.Mobile {
		r.SiteNote = "the allowance and the longest session set from the cloud are left out on a fixed line"
		return r
	}
	merged := r
	if q := o.Video.QuotaMB; q != nil {
		merged.App.Video.Quota = int64(*q) << 20
	}
	if m := o.Video.MaxS; m != nil {
		merged.App.Video.MaxLength = time.Duration(*m) * time.Second
	}
	if err := merged.App.Validate(); err != nil {
		r.SiteErr = err
		return r
	}
	return merged
}

func parse(data []byte) (Result, error) {
	var f file
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Result{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if dec.More() {
		return Result{}, errors.New("invalid JSON: trailing data")
	}
	if f.StateDir == "" {
		return Result{}, errors.New("state_dir is required (where the month's usage is kept)")
	}
	r := Result{BrokerURL: f.BrokerURL, ClientID: f.ClientID, StateDir: f.StateDir, Go2rtc: f.Go2rtc, Sources: map[string]string{}}
	if r.BrokerURL == "" {
		r.BrokerURL = "mqtt://127.0.0.1:1883"
	}
	if r.ClientID == "" {
		r.ClientID = "safehome-video-" + f.Site
	}
	if r.Go2rtc == "" {
		r.Go2rtc = "/usr/local/bin/go2rtc"
	}
	r.FFmpeg = f.FFmpeg
	if r.FFmpeg == "" {
		r.FFmpeg = "/usr/bin/ffmpeg"
	}
	r.PhotoDir = filepath.Join(f.StateDir, "photos")
	var err error
	if r.LogLevel, err = parseLevel(f.LogLevel); err != nil {
		return Result{}, err
	}

	var cams []video.Camera
	for i, c := range f.Cameras {
		where := fmt.Sprintf("cameras[%d]", i)
		if !cameraID.MatchString(c.ID) {
			return Result{}, fmt.Errorf("%s: id %q: lower-case letters, digits and _, 32 at most, such as cam_caisse", where, c.ID)
		}
		if _, ok := r.Sources[c.ID]; ok {
			return Result{}, fmt.Errorf("%s: id %q is repeated", where, c.ID)
		}
		if err := checkSource(c.Source); err != nil {
			return Result{}, fmt.Errorf("%s (%s): source: %w", where, c.ID, err)
		}
		r.Sources[c.ID] = c.Source
		cams = append(cams, video.Camera{ID: c.ID, Name: c.Name})
	}
	if len(cams) == 0 {
		return Result{}, errors.New("cameras: at least one camera (a site without camera does not run the video service)")
	}

	v := video.DefaultConfig(cams)
	switch f.Uplink {
	case "fixed", "":
		if (f.QuotaMB != nil && *f.QuotaMB != 0) || (f.MaxSession != nil && *f.MaxSession != "" && *f.MaxSession != "0") {
			return Result{}, errors.New(`quota_mb and max_session apply only to "uplink": "mobile" (on a fixed line, watching costs the client nothing)`)
		}
	case "mobile":
		// Economy: the data is paid by the client.
		v.Mobile, v.Quota, v.MaxLength, v.Photos = true, video.MobileQuota, video.MobileMaxLength, video.EconomyPhotos
		if q := f.QuotaMB; q != nil {
			if *q < 0 {
				return Result{}, errors.New("quota_mb cannot be negative (0: no allowance)")
			}
			v.Quota = int64(*q) << 20
		}
		if m := f.MaxSession; m != nil && *m != "" {
			if *m == "0" {
				v.MaxLength = 0
			} else if err := duration("max_session", *m, &v.MaxLength); err != nil {
				return Result{}, err
			}
		}
	default:
		return Result{}, fmt.Errorf(`uplink %q: use "fixed" (the shop's box) or "mobile" (a 4G router)`, f.Uplink)
	}
	if f.Photos != nil && !*f.Photos {
		v.Photos = video.PhotoPolicy{}
	}
	if f.MaxSessions != nil {
		v.MaxSessions = *f.MaxSessions
	}
	for _, d := range []struct {
		name, value string
		dst         *time.Duration
	}{{"start_timeout", f.StartTimeout, &v.StartTimeout}, {"stall_after", f.StallAfter, &v.StallAfter}, {"probe_every", f.ProbeEvery, &v.ProbeEvery}} {
		if err := duration(d.name, d.value, d.dst); err != nil {
			return Result{}, err
		}
	}
	r.App = videoapp.DefaultConfig(f.Site, v)
	if err := r.App.Validate(); err != nil {
		return Result{}, err
	}
	return r, nil
}

// checkSource accepts an RTSP address. The error never repeats it: it may hold a
// password.
func checkSource(s string) error {
	u, err := url.Parse(s)
	switch {
	case s == "":
		return errors.New("required (rtsp://user:password@address/path, the camera's secondary stream)")
	case err != nil || u.Hostname() == "":
		return errors.New("not a valid address (rtsp://user:password@address/path)")
	case u.Scheme != "rtsp" && u.Scheme != "rtsps":
		return fmt.Errorf("scheme %q: use rtsp:// or rtsps://", u.Scheme)
	}
	return nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("log_level %q: use debug, info, warn or error", s)
}

func duration(name, value string, dst *time.Duration) error {
	if value == "" {
		return nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s %q: %w (write for example 45s, 2m or 1h30m)", name, value, err)
	}
	*dst = d
	return nil
}
