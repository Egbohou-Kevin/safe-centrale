package video

import "time"

// Input is what the service is told. Every input carries the time it happened.
type Input interface{ at() time.Time }

// Request asks for a session: push one camera's stream to the cloud relay.
type Request struct {
	Now       time.Time
	Session   string        // chosen by the cloud
	Camera    string        // our camera id
	MaxLength time.Duration // asked by the cloud; 0: no limit of its own
	Exp       time.Time     // a request seen after Exp is refused
	// Target is where to push, credential included. The core hands it to Start and
	// never keeps it in its saved state.
	Target string
}

// Stop asks to end a session (video.stop: nobody watches any more).
type Stop struct {
	Now     time.Time
	Session string
}

// Progress says how many bytes a session has sent so far, in all.
type Progress struct {
	Now     time.Time
	Session string
	Bytes   int64
}

// StreamFailed says a session's stream could not start, or broke.
type StreamFailed struct {
	Now     time.Time
	Session string
	Reason  Reason // CameraUnreachable or RelayUnreachable
}

// Probe says whether a camera answers on the local network.
type Probe struct {
	Now       time.Time
	Camera    string
	Reachable bool
}

// Alarm says the alarm went off (alarm.triggered): time for photos.
type Alarm struct {
	Now      time.Time
	Incident string
}

// PhotosTaken says how many bytes of photos were taken, which count in the month's
// usage.
type PhotosTaken struct {
	Now   time.Time
	Bytes int64
}

// Tick says time has passed.
type Tick struct{ Now time.Time }

func (i Request) at() time.Time      { return i.Now }
func (i Stop) at() time.Time         { return i.Now }
func (i Progress) at() time.Time     { return i.Now }
func (i StreamFailed) at() time.Time { return i.Now }
func (i Probe) at() time.Time        { return i.Now }
func (i Tick) at() time.Time         { return i.Now }
func (i Alarm) at() time.Time        { return i.Now }
func (i PhotosTaken) at() time.Time  { return i.Now }

// Action is what the core asks the application to do.
type Action interface{ action() }

// Start: start pushing the camera's stream to Target.
type Start struct {
	Session string
	Camera  string
	Target  string
}

// Halt: stop the session's stream, whatever its state.
type Halt struct{ Session string }

// Report: tell the cloud how a session goes (video.session).
type Report struct {
	Session  string
	Camera   string
	Status   Status
	Reason   Reason        // for Ended and Failed
	Duration time.Duration // for Ended: how long it was live
	Bytes    int64         // for Ended and Failed: data sent
}

// CheckCameras: find out which cameras answer.
type CheckCameras struct{}

// TakePhotos: take Count pictures Every apart from each camera, Width pixels wide,
// and keep them for the link agent to send.
type TakePhotos struct {
	Incident string
	Cameras  []string
	Count    int
	Every    time.Duration
	Width    int
}

// NoPhotos: the alarm gets no photos, for the logs.
type NoPhotos struct {
	Incident string
	Why      string
}

func (TakePhotos) action()   {}
func (NoPhotos) action()     {}
func (Start) action()        {}
func (Halt) action()         {}
func (Report) action()       {}
func (CheckCameras) action() {}

// Status is the step of a session.
type Status string

const (
	Starting Status = "starting"
	Live     Status = "live"
	Ended    Status = "ended"
	Failed   Status = "failed"
)

// Reason says why a session ended or failed.
type Reason string

const (
	Stopped           Reason = "stopped"            // asked by the cloud
	Timeout           Reason = "timeout"            // its longest length reached
	Quota             Reason = "quota"              // the month's allowance is used up
	Busy              Reason = "busy"               // too many sessions at once
	UnknownCamera     Reason = "unknown_camera"     // no such camera
	CameraUnreachable Reason = "camera_unreachable" // the camera does not answer
	RelayUnreachable  Reason = "relay_unreachable"  // nothing reaches the relay
	Expired           Reason = "expired"            // the request came too late
)

// CameraState is a camera and whether it answered the last probe.
type CameraState struct {
	ID        string
	Name      string
	Reachable bool
}

// State is what video.status says.
type State struct {
	Cameras   []CameraState
	Active    int
	Mobile    bool
	Used      int64         // bytes sent this month
	Quota     int64         // bytes per month, 0 for none
	MaxLength time.Duration // longest session set by the client, 0 for none
}
