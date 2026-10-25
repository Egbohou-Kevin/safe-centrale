// Package video is the core of the site's video service (see docs/video.md): which
// live sessions may start, how long they may last, and the month's data allowance
// on mobile data. The stream itself is someone else's business: the application
// (internal/videoapp) starts and stops it and says how many bytes it sent.
//
// Video never takes part in the alarm. Like the other domain cores, this package
// does no I/O, never reads the clock and knows nothing of the contract: inputs carry
// their time, and internal/video/wire translates.
package video

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// Camera is a camera of the site. Its address stays with the application.
type Camera struct {
	ID   string
	Name string
}

// Config holds the service's settings.
type Config struct {
	Cameras []Camera
	// Mobile: the site reaches the internet over mobile data paid by the client, who
	// sets the month's allowance and the longest session. On a fixed line both are 0.
	Mobile    bool
	Quota     int64         // bytes per month, 0 for none
	MaxLength time.Duration // longest session, 0 for none

	MaxSessions int // at once on the site, to spare the uplink
	// StartTimeout is how long a session may take to send its first bytes to the relay.
	StartTimeout time.Duration
	// StallAfter: a live session that sends nothing for this long has lost the relay.
	StallAfter time.Duration
	ProbeEvery time.Duration // how often the cameras are checked

	Photos PhotoPolicy // the pictures taken when the alarm goes off
}

// PhotoPolicy says which pictures are taken when the alarm goes off: Count per
// camera, Every apart, Width pixels wide, for PerHour alarms an hour at most (a
// sensor that triggers again and again must not fill the uplink). Count 0: none.
type PhotoPolicy struct {
	Count   int
	Every   time.Duration
	Width   int
	PerHour int
}

// The photos of docs/video.md: three per camera on a fixed line, one smaller on
// mobile data paid by the client (economy). At most MaxPhotoCameras cameras.
var (
	FixedPhotos   = PhotoPolicy{Count: 3, Every: 2 * time.Second, Width: 640, PerHour: 6}
	EconomyPhotos = PhotoPolicy{Count: 1, Width: 480, PerHour: 6}
)

const (
	MaxPhotoCameras = 4
	maxPhotoCount   = 5
	photoMemory     = 20 // incidents remembered, so that one alarm gets one set of photos
)

// DefaultConfig returns the defaults of docs/video.md for a fixed line.
func DefaultConfig(cameras []Camera) Config {
	return Config{
		Cameras: cameras, MaxSessions: 2,
		StartTimeout: 20 * time.Second, StallAfter: 30 * time.Second, ProbeEvery: 5 * time.Minute,
		Photos: FixedPhotos,
	}
}

// MobileDefaults are the allowance and the longest session proposed at installation
// on mobile data: the client may change them.
const (
	MobileQuota     = 1 << 30 // 1 GB a month
	MobileMaxLength = 5 * time.Minute
)

// Validate checks the settings.
func (c Config) Validate() error {
	if len(c.Cameras) == 0 {
		return errors.New("video: no camera")
	}
	seen := map[string]bool{}
	for _, cam := range c.Cameras {
		if cam.ID == "" || seen[cam.ID] {
			return fmt.Errorf("video: empty or repeated camera id %q", cam.ID)
		}
		seen[cam.ID] = true
	}
	switch {
	case c.Quota < 0 || c.MaxLength < 0:
		return errors.New("video: the allowance and the longest session cannot be negative")
	case !c.Mobile && (c.Quota != 0 || c.MaxLength != 0):
		return errors.New("video: an allowance or a longest session only applies on mobile data")
	case c.MaxSessions < 1:
		return errors.New("video: MaxSessions must be at least 1")
	case c.StartTimeout <= 0 || c.StallAfter <= 0 || c.ProbeEvery <= 0:
		return errors.New("video: StartTimeout, StallAfter and ProbeEvery must be positive")
	}
	return c.Photos.Validate()
}

// Validate checks the policy.
func (p PhotoPolicy) Validate() error {
	switch {
	case p.Count < 0 || p.Count > maxPhotoCount:
		return fmt.Errorf("video: from 0 to %d photos per camera", maxPhotoCount)
	case p.Count == 0:
		return nil
	case p.Width < 160 || p.Width > 1920:
		return errors.New("video: photos from 160 to 1920 pixels wide")
	case p.Every < 0 || (p.Count > 1 && p.Every < 500*time.Millisecond):
		return errors.New("video: photos at least half a second apart")
	case p.PerHour < 1:
		return errors.New("video: photos for at least one alarm an hour")
	}
	return nil
}

// Snapshot is the state saved after every change. The sessions are not in it: a
// restarted service has lost their streams, and the relay sees them end.
type Snapshot struct {
	Month     string          `json:"month"` // "2026-10", in UTC (Togo's time)
	Used      int64           `json:"used"`  // bytes sent this month
	Reachable map[string]bool `json:"reachable"`
	NextProbe time.Time       `json:"next_probe"`
	// Photographed lists the last alarms photographed, PhotosAt when, to keep to
	// PerHour.
	Photographed []string    `json:"photographed,omitempty"`
	PhotosAt     []time.Time `json:"photos_at,omitempty"`
}

// session is a session under way.
type session struct {
	camera    string
	maxLength time.Duration // 0: no limit
	started   time.Time
	liveAt    time.Time // zero while starting
	bytes     int64
	grewAt    time.Time // the last time bytes went out
}

// Service is the core.
type Service struct {
	cfg      Config
	s        Snapshot
	sessions map[string]*session
	// unsaved are the bytes of the sessions under way not yet in s.Used: the state
	// is saved after every change, and counting each progress there would rewrite
	// the file on the SD card every 2 s of viewing. They join it every usedStep, and
	// when a session ends; a crash loses less than usedStep of the count.
	unsaved int64
}

// usedStep is how many bytes of a session under way join the saved usage at once.
const usedStep = 10 << 20

// New builds the core.
func New(cfg Config) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	v := &Service{cfg: cfg, sessions: map[string]*session{}}
	v.Restore(Snapshot{})
	return v, nil
}

// Snapshot returns the state to save.
func (v *Service) Snapshot() Snapshot {
	s := v.s
	s.Reachable = maps.Clone(s.Reachable)
	s.Photographed = slices.Clone(s.Photographed)
	s.PhotosAt = slices.Clone(s.PhotosAt)
	return s
}

// Restore puts a saved state back.
func (v *Service) Restore(s Snapshot) {
	if s.Reachable == nil {
		s.Reachable = map[string]bool{}
	}
	v.s, v.unsaved = s, 0
}

// asap is a wake-up time already past: check the cameras at once.
var asap = time.Unix(0, 0).UTC()

// NextWake returns when the core wants a tick.
func (v *Service) NextWake() time.Time {
	next := v.s.NextProbe
	if next.IsZero() {
		return asap
	}
	for _, s := range v.sessions {
		for _, t := range v.deadlines(s) {
			if t.Before(next) {
				next = t
			}
		}
	}
	return next
}

// deadlines lists the times at which a session may have to end.
func (v *Service) deadlines(s *session) []time.Time {
	if s.liveAt.IsZero() {
		return []time.Time{s.started.Add(v.cfg.StartTimeout)}
	}
	out := []time.Time{s.grewAt.Add(v.cfg.StallAfter)}
	if s.maxLength > 0 {
		out = append(out, s.liveAt.Add(s.maxLength))
	}
	return out
}

// State returns what video.status says.
func (v *Service) State() State {
	st := State{Active: len(v.sessions), Mobile: v.cfg.Mobile, Used: v.used(), Quota: v.cfg.Quota, MaxLength: v.cfg.MaxLength}
	for _, c := range v.cfg.Cameras {
		st.Cameras = append(st.Cameras, CameraState{ID: c.ID, Name: c.Name, Reachable: v.s.Reachable[c.ID]})
	}
	return st
}

// Handle processes one input. The usage starts again from zero in a later month
// only: a clock set back (a dead clock module at boot, corrected later by the
// network) must not wipe the usage the client already paid for this month, twice.
func (v *Service) Handle(in Input) []Action {
	now := in.at()
	if month := now.UTC().Format("2006-01"); month > v.s.Month {
		v.s.Month, v.s.Used, v.unsaved = month, 0, 0
	}
	switch in := in.(type) {
	case Request:
		return v.request(in)
	case Stop:
		if _, ok := v.sessions[in.Session]; ok {
			return v.end(in.Session, now, Stopped)
		}
	case Progress:
		return v.progress(in)
	case StreamFailed:
		if _, ok := v.sessions[in.Session]; ok {
			return v.end(in.Session, now, in.Reason)
		}
	case Alarm:
		return v.alarm(in)
	case PhotosTaken:
		if in.Bytes > 0 {
			v.s.Used += in.Bytes // on mobile data, the client pays for them too
		}
	case Probe:
		if v.hasCamera(in.Camera) {
			v.s.Reachable[in.Camera] = in.Reachable
		}
	case Tick:
		return v.tick(now)
	}
	return nil
}

func (v *Service) request(r Request) []Action {
	if _, ok := v.sessions[r.Session]; ok {
		return nil // the same request again
	}
	refuse := func(why Reason) []Action {
		return []Action{Report{Session: r.Session, Camera: r.Camera, Status: Failed, Reason: why}}
	}
	switch {
	case r.Now.After(r.Exp):
		return refuse(Expired)
	case !v.hasCamera(r.Camera):
		return refuse(UnknownCamera)
	case len(v.sessions) >= v.cfg.MaxSessions:
		return refuse(Busy)
	case v.overQuota():
		return refuse(Quota)
	}
	v.sessions[r.Session] = &session{camera: r.Camera, maxLength: shortest(r.MaxLength, v.cfg.MaxLength), started: r.Now}
	return []Action{
		Start{Session: r.Session, Camera: r.Camera, Target: r.Target},
		Report{Session: r.Session, Camera: r.Camera, Status: Starting},
	}
}

func (v *Service) hasCamera(id string) bool {
	return slices.ContainsFunc(v.cfg.Cameras, func(c Camera) bool { return c.ID == id })
}

// shortest returns the shorter of two limits, where 0 means none.
func shortest(a, b time.Duration) time.Duration {
	if a == 0 || (b != 0 && b < a) {
		return b
	}
	return a
}

func (v *Service) overQuota() bool { return v.cfg.Quota > 0 && v.used() >= v.cfg.Quota }

// used is the month's usage, sessions under way included.
func (v *Service) used() int64 { return v.s.Used + v.unsaved }

// keepUsed puts the bytes counted since the last save into the saved usage.
func (v *Service) keepUsed() { v.s.Used, v.unsaved = v.used(), 0 }

func (v *Service) progress(p Progress) []Action {
	s, ok := v.sessions[p.Session]
	if !ok || p.Bytes <= s.bytes {
		return nil
	}
	v.unsaved += p.Bytes - s.bytes
	if v.unsaved >= usedStep {
		v.keepUsed()
	}
	s.bytes, s.grewAt = p.Bytes, p.Now
	var acts []Action
	if s.liveAt.IsZero() {
		s.liveAt = p.Now
		acts = append(acts, Report{Session: p.Session, Camera: s.camera, Status: Live})
	}
	if v.overQuota() {
		acts = append(acts, v.end(p.Session, p.Now, Quota)...)
	}
	return acts
}

func (v *Service) tick(now time.Time) []Action {
	var acts []Action
	for _, id := range slices.Sorted(maps.Keys(v.sessions)) {
		s := v.sessions[id]
		switch {
		case s.liveAt.IsZero() && !now.Before(s.started.Add(v.cfg.StartTimeout)):
			acts = append(acts, v.end(id, now, RelayUnreachable)...)
		case s.liveAt.IsZero():
		case s.maxLength > 0 && !now.Before(s.liveAt.Add(s.maxLength)):
			acts = append(acts, v.end(id, now, Timeout)...)
		case !now.Before(s.grewAt.Add(v.cfg.StallAfter)):
			acts = append(acts, v.end(id, now, RelayUnreachable)...)
		}
	}
	if !now.Before(v.s.NextProbe) {
		v.s.NextProbe = now.Add(v.cfg.ProbeEvery)
		acts = append(acts, CheckCameras{})
	}
	return acts
}

// end stops a session and reports it: ended if it went live or the cloud stopped
// it, failed otherwise.
func (v *Service) end(id string, now time.Time, why Reason) []Action {
	s := v.sessions[id]
	delete(v.sessions, id)
	v.keepUsed()
	r := Report{Session: id, Camera: s.camera, Status: Failed, Reason: why, Bytes: s.bytes}
	if why == Stopped {
		r.Status = Ended
	}
	if !s.liveAt.IsZero() {
		r.Status, r.Duration = Ended, now.Sub(s.liveAt)
	}
	return []Action{Halt{Session: id}, r}
}
