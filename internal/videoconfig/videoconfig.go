// Package videoconfig reads the video service's configuration file (JSON), strictly:
// a misspelt key is an error. The cameras' addresses, passwords included, are in it:
// the file is readable by the service only (see docs/systeme.md), and nothing here
// prints them. See config/video.example.json.
package videoconfig

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"time"

	"safehome/internal/configfile"
	"safehome/internal/contract"
	"safehome/internal/siteconfig"
	"safehome/internal/video"
	"safehome/internal/videoapp"
)

// Result is what the file describes.
type Result struct {
	configfile.Program
	App      videoapp.Config
	Sources  map[string]string // camera id -> its RTSP address; never printed
	Go2rtc   string            // the go2rtc executable
	FFmpeg   string            // decodes the photos' pictures
	PhotoDir string            // where the photos wait for the link agent

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
	configfile.ProgramFile
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
	var f file
	if err := configfile.Decode(path, &f); err != nil {
		return Result{}, fmt.Errorf("videoconfig: %w", err)
	}
	r, err := build(f)
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
		var err error
		if merged.App.Video.Quota, err = quota(*q); err != nil {
			r.SiteErr = err
			return r
		}
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

func build(f file) (Result, error) {
	p, err := f.Build("safehome-video-", "where the month's usage is kept")
	if err != nil {
		return Result{}, err
	}
	r := Result{
		Program:  p,
		Go2rtc:   cmp.Or(f.Go2rtc, "/usr/local/bin/go2rtc"),
		FFmpeg:   cmp.Or(f.FFmpeg, "/usr/bin/ffmpeg"),
		PhotoDir: filepath.Join(f.StateDir, "photos"),
	}
	var cams []video.Camera
	if r.Sources, cams, err = parseCameras(f.Cameras); err != nil {
		return Result{}, err
	}
	v, err := videoSettings(f, cams)
	if err != nil {
		return Result{}, err
	}
	r.App = videoapp.DefaultConfig(f.Site, v)
	if err := r.App.Validate(); err != nil {
		return Result{}, err
	}
	return r, nil
}

// parseCameras checks the cameras: at least one, each with its own id and an RTSP
// address. The errors never repeat an address.
func parseCameras(list []cameraFile) (map[string]string, []video.Camera, error) {
	sources := map[string]string{}
	var cams []video.Camera
	for i, c := range list {
		where := fmt.Sprintf("cameras[%d]", i)
		if !cameraID.MatchString(c.ID) {
			return nil, nil, fmt.Errorf("%s: id %q: lower-case letters, digits and _, 32 at most, such as cam_caisse", where, c.ID)
		}
		if _, ok := sources[c.ID]; ok {
			return nil, nil, fmt.Errorf("%s: id %q is repeated", where, c.ID)
		}
		if err := checkSource(c.Source); err != nil {
			return nil, nil, fmt.Errorf("%s (%s): source: %w", where, c.ID, err)
		}
		sources[c.ID] = c.Source
		cams = append(cams, video.Camera{ID: c.ID, Name: c.Name})
	}
	if len(cams) == 0 {
		return nil, nil, errors.New("cameras: at least one camera (a site without camera does not run the video service)")
	}
	return sources, cams, nil
}

// videoSettings builds the core's settings: the defaults for the uplink, then what
// the file changes.
func videoSettings(f file, cams []video.Camera) (video.Config, error) {
	v := video.DefaultConfig(cams)
	if err := uplink(f, &v); err != nil {
		return v, err
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
		if err := configfile.Duration(d.name, d.value, d.dst); err != nil {
			return v, err
		}
	}
	return v, nil
}

// uplink sets what depends on the uplink: on mobile data, paid by the client, an
// allowance, a longest session and fewer photos (economy).
func uplink(f file, v *video.Config) error {
	switch f.Uplink {
	case "fixed", "":
		if (f.QuotaMB != nil && *f.QuotaMB != 0) || (f.MaxSession != nil && *f.MaxSession != "" && *f.MaxSession != "0") {
			return errors.New(`quota_mb and max_session apply only to "uplink": "mobile" (on a fixed line, watching costs the client nothing)`)
		}
		return nil
	case "mobile":
	default:
		return fmt.Errorf(`uplink %q: use "fixed" (the shop's box) or "mobile" (a 4G router)`, f.Uplink)
	}
	v.Mobile, v.Quota, v.MaxLength, v.Photos = true, video.MobileQuota, video.MobileMaxLength, video.EconomyPhotos
	if q := f.QuotaMB; q != nil {
		var err error
		if v.Quota, err = quota(*q); err != nil {
			return err
		}
	}
	switch m := f.MaxSession; {
	case m == nil || *m == "":
	case *m == "0":
		v.MaxLength = 0
	default:
		return configfile.Duration("max_session", *m, &v.MaxLength)
	}
	return nil
}

// quota turns quota_mb into bytes. The bound is the contract's (contract.MaxQuotaMB):
// far above any client's, it never overflows once in bytes.
func quota(mb int) (int64, error) {
	if mb < 0 || mb > contract.MaxQuotaMB {
		return 0, fmt.Errorf("quota_mb from 0 (no allowance) to %d", contract.MaxQuotaMB)
	}
	return int64(mb) << 20, nil
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
