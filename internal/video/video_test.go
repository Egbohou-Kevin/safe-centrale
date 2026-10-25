package video

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)

var cameras = []Camera{{ID: "cam_caisse", Name: "Caisse"}, {ID: "cam_cour", Name: "Cour"}}

type harness struct {
	t   *testing.T
	v   *Service
	now time.Time
}

func newHarness(t *testing.T, edit ...func(*Config)) *harness {
	t.Helper()
	cfg := DefaultConfig(cameras)
	for _, e := range edit {
		e(&cfg)
	}
	v, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, v: v, now: t0}
	h.v.Handle(Tick{Now: h.now}) // the first camera check
	return h
}

func mobile(c *Config) { c.Mobile, c.Quota, c.MaxLength = true, MobileQuota, MobileMaxLength }

func (h *harness) wait(d time.Duration) []Action {
	h.now = h.now.Add(d)
	return h.v.Handle(Tick{Now: h.now})
}

func (h *harness) request(session, camera string, maxLength time.Duration) []Action {
	return h.v.Handle(Request{Now: h.now, Session: session, Camera: camera, MaxLength: maxLength, Exp: h.now.Add(30 * time.Second), Target: "secret-target"})
}

func (h *harness) progress(session string, bytes int64) []Action {
	return h.v.Handle(Progress{Now: h.now, Session: session, Bytes: bytes})
}

func reports(acts []Action) []Report {
	var out []Report
	for _, a := range acts {
		if r, ok := a.(Report); ok {
			out = append(out, r)
		}
	}
	return out
}

func only(t *testing.T, acts []Action) Report {
	t.Helper()
	r := reports(acts)
	if len(r) != 1 {
		t.Fatalf("want one report, got %+v", acts)
	}
	return r[0]
}

func halted(acts []Action, session string) bool {
	for _, a := range acts {
		if h, ok := a.(Halt); ok && h.Session == session {
			return true
		}
	}
	return false
}

func TestASessionStartsGoesLiveAndIsStopped(t *testing.T) {
	h := newHarness(t)
	acts := h.request("s1", "cam_caisse", 0)
	if !reflect.DeepEqual(acts, []Action{
		Start{Session: "s1", Camera: "cam_caisse", Target: "secret-target"},
		Report{Session: "s1", Camera: "cam_caisse", Status: Starting},
	}) {
		t.Fatalf("%+v", acts)
	}
	if h.v.State().Active != 1 {
		t.Fatal("one session under way")
	}
	h.now = h.now.Add(2 * time.Second)
	if r := only(t, h.progress("s1", 40_000)); r.Status != Live {
		t.Fatalf("%+v", r)
	}
	if acts := h.progress("s1", 40_000); acts != nil {
		t.Fatalf("no new bytes, nothing to say: %+v", acts)
	}
	h.now = h.now.Add(10 * time.Second)
	h.progress("s1", 500_000)
	h.now = h.now.Add(5 * time.Second)
	acts = h.v.Handle(Stop{Now: h.now, Session: "s1"})
	r := only(t, acts)
	if !halted(acts, "s1") || r.Status != Ended || r.Reason != Stopped || r.Duration != 15*time.Second || r.Bytes != 500_000 {
		t.Fatalf("%+v", acts)
	}
	if st := h.v.State(); st.Active != 0 || st.Used != 500_000 {
		t.Fatalf("%+v", st)
	}
	if acts := h.v.Handle(Stop{Now: h.now, Session: "s1"}); acts != nil {
		t.Fatalf("a second stop does nothing: %+v", acts)
	}
}

func TestRefusals(t *testing.T) {
	h := newHarness(t)
	late := h.v.Handle(Request{Now: h.now, Session: "s0", Camera: "cam_caisse", Exp: h.now.Add(-time.Second), Target: "x"})
	if r := only(t, late); r.Status != Failed || r.Reason != Expired {
		t.Fatalf("%+v", r)
	}
	if r := only(t, h.request("s1", "cam_toilettes", 0)); r.Reason != UnknownCamera || r.Camera != "cam_toilettes" {
		t.Fatalf("%+v", r)
	}
	h.request("s2", "cam_caisse", 0)
	if acts := h.request("s2", "cam_caisse", 0); acts != nil {
		t.Fatalf("the same request again is ignored: %+v", acts)
	}
	h.request("s3", "cam_cour", 0)
	if r := only(t, h.request("s4", "cam_caisse", 0)); r.Reason != Busy {
		t.Fatalf("two sessions at most: %+v", r)
	}
	if h.v.State().Active != 2 {
		t.Fatal("the refused requests start nothing")
	}
}

func TestAStopDuringTheStartEndsTheSession(t *testing.T) {
	h := newHarness(t)
	h.request("s1", "cam_caisse", 0)
	acts := h.v.Handle(Stop{Now: h.now, Session: "s1"})
	if r := only(t, acts); !halted(acts, "s1") || r.Status != Ended || r.Reason != Stopped || r.Duration != 0 {
		t.Fatalf("%+v", acts)
	}
}

func TestNothingReachesTheRelay(t *testing.T) {
	h := newHarness(t)
	h.request("s1", "cam_caisse", 0)
	if acts := h.wait(19 * time.Second); len(acts) != 0 {
		t.Fatalf("too early: %+v", acts)
	}
	acts := h.wait(time.Second)
	if r := only(t, acts); !halted(acts, "s1") || r.Status != Failed || r.Reason != RelayUnreachable {
		t.Fatalf("%+v", acts)
	}
}

func TestTheCameraDoesNotAnswer(t *testing.T) {
	h := newHarness(t)
	h.request("s1", "cam_caisse", 0)
	acts := h.v.Handle(StreamFailed{Now: h.now, Session: "s1", Reason: CameraUnreachable})
	if r := only(t, acts); r.Status != Failed || r.Reason != CameraUnreachable {
		t.Fatalf("%+v", acts)
	}
	if acts := h.v.Handle(StreamFailed{Now: h.now, Session: "s1", Reason: CameraUnreachable}); acts != nil {
		t.Fatalf("a late failure of a finished session does nothing: %+v", acts)
	}
}

func TestALiveSessionThatBreaksHasEnded(t *testing.T) {
	h := newHarness(t)
	h.request("s1", "cam_caisse", 0)
	h.progress("s1", 1000)
	h.now = h.now.Add(4 * time.Second)
	acts := h.v.Handle(StreamFailed{Now: h.now, Session: "s1", Reason: RelayUnreachable})
	if r := only(t, acts); r.Status != Ended || r.Reason != RelayUnreachable || r.Duration != 4*time.Second || r.Bytes != 1000 {
		t.Fatalf("%+v", acts)
	}
}

func TestALiveSessionThatStallsHasLostTheRelay(t *testing.T) {
	h := newHarness(t)
	h.request("s1", "cam_caisse", 0)
	h.progress("s1", 1000)
	h.wait(20 * time.Second)
	h.progress("s1", 2000)
	if acts := h.wait(29 * time.Second); len(acts) != 0 {
		t.Fatalf("bytes went out 29 s ago: %+v", acts)
	}
	if r := only(t, h.wait(time.Second)); r.Status != Ended || r.Reason != RelayUnreachable || r.Duration != 50*time.Second {
		t.Fatalf("%+v", r)
	}
}

func TestTheLongestSessionIsTheShorterLimit(t *testing.T) {
	// On mobile data the client's 5 minutes, unless the cloud asks for less.
	for _, c := range []struct {
		asked time.Duration
		want  time.Duration
	}{{0, 5 * time.Minute}, {2 * time.Minute, 2 * time.Minute}, {time.Hour, 5 * time.Minute}} {
		h := newHarness(t, mobile)
		h.request("s1", "cam_caisse", c.asked)
		h.progress("s1", 1)
		var got time.Duration
		for range 20 {
			acts := h.wait(15 * time.Second)
			h.progress("s1", int64(h.now.Sub(t0)/time.Second))
			if r := reports(acts); len(r) == 1 {
				if r[0].Reason != Timeout || r[0].Status != Ended {
					t.Fatalf("%+v", r)
				}
				got = r[0].Duration
				break
			}
		}
		if got != c.want {
			t.Errorf("asked %v: ended after %v, want %v", c.asked, got, c.want)
		}
	}
	// On a fixed line, only the cloud's limit.
	h := newHarness(t)
	h.request("s1", "cam_caisse", time.Minute)
	h.progress("s1", 1)
	h.wait(25 * time.Second)
	h.progress("s1", 2)
	if r := only(t, h.wait(35*time.Second)); r.Reason != Timeout || r.Duration != time.Minute {
		t.Fatalf("%+v", r)
	}
	h.request("s2", "cam_caisse", 0)
	h.progress("s2", 1)
	for range 100 {
		if acts := h.wait(20 * time.Second); len(acts) > 0 && len(reports(acts)) > 0 {
			t.Fatalf("no limit on a fixed line: %+v", acts)
		}
		h.progress("s2", int64(h.now.Sub(t0)/time.Second))
	}
}

func TestTheAllowanceStopsAndRefusesThenComesBackNextMonth(t *testing.T) {
	h := newHarness(t, func(c *Config) { mobile(c); c.Quota = 1_000_000 })
	h.request("s1", "cam_caisse", 0)
	h.progress("s1", 600_000)
	h.request("s2", "cam_cour", 0)
	h.progress("s2", 300_000)
	acts := h.progress("s1", 700_000) // 1 000 000 in all
	if r := only(t, acts); !halted(acts, "s1") || r.Reason != Quota || r.Status != Ended {
		t.Fatalf("%+v", acts)
	}
	if r := only(t, h.progress("s2", 300_001)); r.Session != "s2" || r.Reason != Quota {
		t.Fatalf("%+v", r)
	}
	if r := only(t, h.request("s3", "cam_caisse", 0)); r.Reason != Quota {
		t.Fatalf("%+v", r)
	}
	if st := h.v.State(); st.Used != 1_000_001 || st.Quota != 1_000_000 || !st.Mobile || st.MaxLength != MobileMaxLength {
		t.Fatalf("%+v", st)
	}
	// The 1st of November, UTC.
	h.now = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if r := only(t, h.request("s4", "cam_caisse", 0)); r.Status != Starting {
		t.Fatalf("%+v", r)
	}
	if h.v.State().Used != 0 {
		t.Fatal("a new month starts from zero")
	}
}

func TestTheUsageSurvivesARestartButNotTheSessions(t *testing.T) {
	h := newHarness(t, mobile)
	h.request("s1", "cam_caisse", 0)
	h.progress("s1", 123_456)
	h.v.Handle(Stop{Now: h.now, Session: "s1"})
	h.v.Handle(Probe{Now: h.now, Camera: "cam_caisse", Reachable: true})
	saved := h.v.Snapshot()

	v, _ := New(h.v.cfg)
	v.Restore(saved)
	if st := v.State(); st.Used != 123_456 || st.Active != 0 || !st.Cameras[0].Reachable {
		t.Fatalf("%+v", st)
	}
	if acts := v.Handle(Progress{Now: h.now, Session: "s1", Bytes: 200_000}); acts != nil {
		t.Fatalf("a session of the previous run is unknown: %+v", acts)
	}
	saved.Reachable["cam_cour"] = true
	if h.v.Snapshot().Reachable["cam_cour"] {
		t.Fatal("the snapshot must be a copy")
	}
}

func TestCamerasAreCheckedAtOnceThenRegularly(t *testing.T) {
	v, _ := New(DefaultConfig(cameras))
	if !v.NextWake().Before(t0) {
		t.Fatal("a fresh service checks the cameras at once")
	}
	if acts := v.Handle(Tick{Now: t0}); !reflect.DeepEqual(acts, []Action{CheckCameras{}}) {
		t.Fatalf("%+v", acts)
	}
	if !v.NextWake().Equal(t0.Add(5 * time.Minute)) {
		t.Fatalf("%v", v.NextWake())
	}
	v.Handle(Probe{Now: t0, Camera: "cam_cour", Reachable: true})
	v.Handle(Probe{Now: t0, Camera: "cam_unknown", Reachable: true})
	st := v.State()
	if st.Cameras[0] != (CameraState{ID: "cam_caisse", Name: "Caisse"}) || !st.Cameras[1].Reachable || len(st.Cameras) != 2 {
		t.Fatalf("%+v", st.Cameras)
	}
	if acts := v.Handle(Tick{Now: t0.Add(4 * time.Minute)}); acts != nil {
		t.Fatalf("%+v", acts)
	}
	if acts := v.Handle(Tick{Now: t0.Add(5 * time.Minute)}); !reflect.DeepEqual(acts, []Action{CheckCameras{}}) {
		t.Fatalf("%+v", acts)
	}
}

func TestNextWakeFollowsTheSessions(t *testing.T) {
	h := newHarness(t, mobile)
	h.request("s1", "cam_caisse", 0)
	if !h.v.NextWake().Equal(t0.Add(20 * time.Second)) {
		t.Fatalf("the start deadline: %v", h.v.NextWake())
	}
	h.now = t0.Add(5 * time.Second)
	h.progress("s1", 10)
	if !h.v.NextWake().Equal(t0.Add(35 * time.Second)) {
		t.Fatalf("the stall deadline: %v", h.v.NextWake())
	}
	h.v.cfg.StallAfter = time.Hour
	if !h.v.NextWake().Equal(t0.Add(5 * time.Minute)) {
		t.Fatalf("the probe, before the longest session: %v", h.v.NextWake())
	}
	h.v.cfg.MaxLength, h.v.sessions["s1"].maxLength = time.Minute, time.Minute
	if !h.v.NextWake().Equal(t0.Add(65 * time.Second)) {
		t.Fatalf("the longest session: %v", h.v.NextWake())
	}
}

func TestTheTargetIsNeverSaved(t *testing.T) {
	h := newHarness(t)
	h.request("s1", "cam_caisse", 0)
	h.progress("s1", 10)
	b, err := json.Marshal(h.v.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "s1") {
		t.Fatalf("the sessions leaked into the saved state: %s", b)
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(DefaultConfig(cameras)); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*Config){
		"no camera":       func(c *Config) { c.Cameras = nil },
		"repeated camera": func(c *Config) { c.Cameras = []Camera{{ID: "a"}, {ID: "a"}} },
		"empty id":        func(c *Config) { c.Cameras = []Camera{{ID: ""}} },
		"quota on fixed":  func(c *Config) { c.Quota = 1 },
		"length on fixed": func(c *Config) { c.MaxLength = time.Minute },
		"negative quota":  func(c *Config) { c.Mobile, c.Quota = true, -1 },
		"no session":      func(c *Config) { c.MaxSessions = 0 },
		"no start wait":   func(c *Config) { c.StartTimeout = 0 },
		"no stall wait":   func(c *Config) { c.StallAfter = 0 },
		"no probe":        func(c *Config) { c.ProbeEvery = 0 },
	} {
		cfg := DefaultConfig(cameras)
		edit(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	cfg := DefaultConfig(cameras)
	mobile(&cfg)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAnAlarmGetsOneSetOfPhotos(t *testing.T) {
	h := newHarness(t)
	acts := h.v.Handle(Alarm{Now: h.now, Incident: "inc1"})
	want := []Action{TakePhotos{Incident: "inc1", Cameras: []string{"cam_caisse", "cam_cour"}, Count: 3, Every: 2 * time.Second, Width: 640}}
	if !reflect.DeepEqual(acts, want) {
		t.Fatalf("%+v", acts)
	}
	if acts := h.v.Handle(Alarm{Now: h.now, Incident: "inc1"}); acts != nil {
		t.Fatalf("the same alarm again: %+v", acts)
	}
	if acts := h.v.Handle(Alarm{Now: h.now}); acts != nil {
		t.Fatal("no incident, no photos")
	}
	// Remembered across a restart.
	v2, _ := New(DefaultConfig(cameras))
	v2.Restore(h.v.Snapshot())
	if acts := v2.Handle(Alarm{Now: h.now, Incident: "inc1"}); acts != nil {
		t.Fatal("photographed before the restart")
	}
}

func TestPhotosAreLimitedPerHour(t *testing.T) {
	h := newHarness(t)
	for i := range 6 {
		if acts := h.v.Handle(Alarm{Now: h.now, Incident: fmt.Sprint("inc", i)}); len(acts) != 1 {
			t.Fatalf("%d: %+v", i, acts)
		}
		h.now = h.now.Add(time.Minute)
	}
	acts := h.v.Handle(Alarm{Now: h.now, Incident: "inc6"})
	if len(acts) != 1 || !strings.Contains(acts[0].(NoPhotos).Why, "6 alarms") {
		t.Fatalf("%+v", acts)
	}
	h.now = t0.Add(time.Hour)
	if acts := h.v.Handle(Alarm{Now: h.now, Incident: "inc7"}); len(acts) != 1 || reflect.TypeOf(acts[0]) != reflect.TypeOf(TakePhotos{}) {
		t.Fatalf("an hour later: %+v", acts)
	}
	if n := len(h.v.Snapshot().PhotosAt); n != 6 {
		t.Fatalf("%d times kept", n)
	}
	for i := range 30 {
		h.v.Handle(Alarm{Now: h.now, Incident: fmt.Sprint("x", i)})
	}
	if n := len(h.v.Snapshot().Photographed); n != photoMemory {
		t.Fatalf("%d incidents kept", n)
	}
}

func TestEconomyPhotosAndManyCameras(t *testing.T) {
	var many []Camera
	for i := range 6 {
		many = append(many, Camera{ID: fmt.Sprint("cam", i)})
	}
	v, err := New(func() Config { c := DefaultConfig(many); mobile(&c); c.Photos = EconomyPhotos; return c }())
	if err != nil {
		t.Fatal(err)
	}
	acts := v.Handle(Alarm{Now: t0, Incident: "i"})
	tp := acts[0].(TakePhotos)
	if len(tp.Cameras) != MaxPhotoCameras || tp.Count != 1 || tp.Width != 480 {
		t.Fatalf("%+v", tp)
	}
	// Past the allowance: photos all the same, and they count.
	v.Handle(PhotosTaken{Now: t0, Bytes: MobileQuota})
	if acts := v.Handle(Alarm{Now: t0, Incident: "j"}); len(acts) != 1 {
		t.Fatalf("%+v", acts)
	}
	if v.State().Used != MobileQuota {
		t.Fatal("the photos count in the month's usage")
	}
	v.Handle(PhotosTaken{Now: t0, Bytes: -5})
	if v.State().Used != MobileQuota {
		t.Fatal("nonsense ignored")
	}
}

func TestNoPhotosWhenSetSo(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Photos = PhotoPolicy{} })
	if acts := h.v.Handle(Alarm{Now: h.now, Incident: "i"}); acts != nil {
		t.Fatalf("%+v", acts)
	}
	for name, p := range map[string]PhotoPolicy{
		"too many":   {Count: 6, Width: 640, PerHour: 1, Every: time.Second},
		"negative":   {Count: -1},
		"too narrow": {Count: 1, Width: 100, PerHour: 1},
		"too wide":   {Count: 1, Width: 4000, PerHour: 1},
		"too close":  {Count: 2, Width: 640, PerHour: 1, Every: 100 * time.Millisecond},
		"never":      {Count: 1, Width: 640},
	} {
		c := DefaultConfig(cameras)
		c.Photos = p
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The service was down while alarms went off (a crash loop, a configuration to fix):
// the broker hands them over hours later. Photos taken now would show the shop as it
// is, not the intrusion, and use up the hour's allowance: a live alarm right after
// still gets its photos.
func TestALateAlarmGetsNoPhotosNorTheHoursAllowance(t *testing.T) {
	h := newHarness(t)
	for i := range 6 {
		acts := h.v.Handle(Alarm{Now: h.now, At: h.now.Add(-5 * time.Hour), Incident: fmt.Sprint("old", i)})
		if len(acts) != 1 || !strings.Contains(acts[0].(NoPhotos).Why, "5h0m0s old") {
			t.Fatalf("%d: %+v", i, acts)
		}
	}
	if acts := h.v.Handle(Alarm{Now: h.now, At: h.now.Add(-time.Minute), Incident: "live"}); len(acts) != 1 || reflect.TypeOf(acts[0]) != reflect.TypeOf(TakePhotos{}) {
		t.Fatalf("a live alarm: %+v", acts)
	}
	// Written by a clock set back since: as if it went off now.
	if acts := h.v.Handle(Alarm{Now: h.now, At: h.now.Add(time.Hour), Incident: "ahead"}); len(acts) != 1 || reflect.TypeOf(acts[0]) != reflect.TypeOf(TakePhotos{}) {
		t.Fatalf("an alarm from the future: %+v", acts)
	}
}
