// Package maint is the core of remote maintenance (see docs/maintenance.md): when
// the hub opens a tunnel to our bastion, for how long, and what it tells the cloud.
// One session at a time, an hour at most, closed when its time is over or when the
// cloud says so. The tunnel itself is the application's business
// (internal/maintapp), which says when it is up and when it broke.
//
// Like the other domain cores, this package does no I/O, never reads the clock and
// knows nothing of the contract: inputs carry their time, internal/maint/wire
// translates.
package maint

import (
	"errors"
	"time"
)

// Config holds the core's settings.
type Config struct {
	// Enabled: the installer may switch remote maintenance off on a hub.
	Enabled bool
	// MaxLength is the longest a session lasts, whatever the cloud asks.
	MaxLength time.Duration
	// RetryAfter: a tunnel that broke while open is opened again after this long, as
	// long as the session lasts.
	RetryAfter time.Duration
}

// DefaultConfig returns the defaults of docs/maintenance.md.
func DefaultConfig() Config {
	return Config{Enabled: true, MaxLength: time.Hour, RetryAfter: 10 * time.Second}
}

// Validate checks the settings.
func (c Config) Validate() error {
	if c.MaxLength < time.Minute || c.MaxLength > time.Hour || c.RetryAfter <= 0 {
		return errors.New("maint: MaxLength from 1 min to 1 h, and a positive RetryAfter")
	}
	return nil
}

// Snapshot is the state saved after every change: a session survives a restart of
// the service, until its time is over.
type Snapshot struct {
	Session  string    `json:"session,omitempty"`
	User     string    `json:"user,omitempty"`
	Until    time.Time `json:"until,omitempty"`
	OpenedAt time.Time `json:"opened_at,omitempty"` // the first time the tunnel was up
}

// Service is the core.
type Service struct {
	cfg     Config
	s       Snapshot
	running bool      // a tunnel is started (not saved: it dies with the process)
	retryAt time.Time // when to start it again after it broke
}

// New builds the core.
func New(cfg Config) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Service{cfg: cfg}, nil
}

// Snapshot returns the state to save.
func (v *Service) Snapshot() Snapshot { return v.s }

// Restore puts a saved state back. The tunnel is not running: Boot decides.
func (v *Service) Restore(s Snapshot) { v.s, v.running, v.retryAt = s, false, time.Time{} }

// NextWake returns when the core wants a tick, zero if never.
func (v *Service) NextWake() time.Time {
	if v.s.Session == "" {
		return time.Time{}
	}
	if !v.retryAt.IsZero() && v.retryAt.Before(v.s.Until) {
		return v.retryAt
	}
	return v.s.Until
}

// Handle processes one input.
func (v *Service) Handle(in Input) []Action {
	switch in := in.(type) {
	case Open:
		return v.open(in)
	case Close:
		if in.Session != "" && in.Session == v.s.Session {
			return v.end(in.Now, Stopped)
		}
	case Up:
		if in.Session == v.s.Session && v.running && v.s.OpenedAt.IsZero() {
			v.s.OpenedAt = in.Now
			return []Action{Report{Session: v.s.Session, User: v.s.User, Status: IsOpen, Until: v.s.Until}}
		}
	case Down:
		return v.down(in)
	case Boot:
		return v.boot(in.Now)
	case Tick:
		return v.tick(in.Now)
	}
	return nil
}

func (v *Service) open(o Open) []Action {
	refuse := func(why Reason) []Action {
		return []Action{Report{Session: o.Session, User: o.User, Status: Failed, Reason: why}}
	}
	switch {
	case o.Session == v.s.Session:
		return nil // the same order again
	case !v.cfg.Enabled:
		return refuse(Disabled)
	case !o.Now.Before(o.Exp):
		return refuse(Expired)
	case v.s.Session != "":
		return refuse(Busy)
	}
	length := min(o.Length, v.cfg.MaxLength)
	v.s = Snapshot{Session: o.Session, User: o.User, Until: o.Now.Add(length)}
	v.running, v.retryAt = true, time.Time{}
	return []Action{
		Start{Session: o.Session},
		Report{Session: o.Session, User: o.User, Status: Opening, Until: v.s.Until},
	}
}

// down: the tunnel stopped by itself. Never up: the bastion did not let the hub in,
// the session fails. Up before: it broke (the internet dropped), it is opened again
// after RetryAfter, as long as the session lasts.
func (v *Service) down(d Down) []Action {
	if d.Session != v.s.Session || !v.running {
		return nil
	}
	v.running = false
	if v.s.OpenedAt.IsZero() {
		r := Report{Session: v.s.Session, User: v.s.User, Status: Failed, Reason: Unreachable}
		v.s = Snapshot{}
		return []Action{r}
	}
	v.retryAt = d.Now.Add(v.cfg.RetryAfter)
	return nil
}

// boot runs once the service started: a session saved before a restart goes on
// until its time, or is said to be over. An order may come before it: the session it
// opened is live already (its tunnel started, or waits to be started again), and is
// left alone.
func (v *Service) boot(now time.Time) []Action {
	if v.s.Session == "" || v.running || !v.retryAt.IsZero() {
		return nil
	}
	if !now.Before(v.s.Until) {
		return v.end(now, Timeout)
	}
	v.running = true
	return []Action{Start{Session: v.s.Session}}
}

func (v *Service) tick(now time.Time) []Action {
	if v.s.Session == "" {
		return nil
	}
	if !now.Before(v.s.Until) {
		return v.end(now, Timeout)
	}
	if !v.running && !v.retryAt.IsZero() && !now.Before(v.retryAt) {
		v.running, v.retryAt = true, time.Time{}
		return []Action{Start{Session: v.s.Session}}
	}
	return nil
}

// end closes the session and says so. The time it was open never goes past its end:
// a session found over after a restart was not open while the service was down.
func (v *Service) end(now time.Time, why Reason) []Action {
	r := Report{Session: v.s.Session, User: v.s.User, Status: Closed, Reason: why}
	if !v.s.OpenedAt.IsZero() {
		r.Duration = min(now.Sub(v.s.OpenedAt), v.s.Until.Sub(v.s.OpenedAt))
	}
	acts := []Action{}
	if v.running {
		acts = append(acts, Stop{Session: v.s.Session})
	}
	v.s, v.running, v.retryAt = Snapshot{}, false, time.Time{}
	return append(acts, r)
}
