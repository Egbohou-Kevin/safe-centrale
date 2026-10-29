package engine

import (
	"fmt"
	"testing"
	"time"
)

// --- harness -------------------------------------------------------------

// Devices of the test shop.
const (
	door   = "dev_door"   // entry door
	back   = "dev_back"   // back door, perimeter
	window = "dev_window" // shop window, glass break
	motion = "dev_motion" // interior motion
	smoke  = "dev_smoke"
	panic_ = "dev_panic"
	tamper = "dev_tamper"
)

var t0 = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

func testConfig() Config {
	c := DefaultConfig()
	c.Zones = map[string]Zone{
		door:   {Name: "entrance", Kind: ZoneEntry},
		back:   {Name: "back", Kind: ZoneInstant},
		window: {Name: "window", Kind: ZoneInstant},
		motion: {Name: "shop", Kind: ZoneInterior},
		smoke:  {Name: "smoke", Kind: ZoneFire},
		panic_: {Name: "counter", Kind: ZonePanic},
		tamper: {Name: "hub", Kind: ZoneTamper},
	}
	return c
}

type harness struct {
	t   *testing.T
	e   *Engine
	now time.Time
	ids int
	cfg Config
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, now: t0, cfg: testConfig()}
	for _, m := range mutate {
		m(&h.cfg)
	}
	e, err := New(h.cfg, func() string { h.ids++; return fmt.Sprintf("inc-%d", h.ids) }, t0)
	if err != nil {
		t.Fatal(err)
	}
	h.e = e
	return h
}

// advance moves the clock forward and sends a tick.
func (h *harness) advance(d time.Duration) []Action {
	h.now = h.now.Add(d)
	return h.e.Handle(Tick{Now: h.now})
}

func (h *harness) signal(dev string, sensor SensorKind, active bool) []Action {
	return h.e.Handle(Signal{Now: h.now, DeviceID: dev, Sensor: sensor, Active: active})
}

func (h *harness) open(dev string) []Action  { return h.signal(dev, SensorContact, true) }
func (h *harness) close(dev string) []Action { return h.signal(dev, SensorContact, false) }
func (h *harness) moves() []Action           { return h.signal(motion, SensorMotion, true) }
func (h *harness) breakWindow() []Action     { return h.signal(window, SensorGlassBreak, true) }

var cmdSeq int

func (h *harness) cmd(name CommandName, mutate ...func(*Command)) []Action {
	cmdSeq++
	c := Command{Now: h.now, ID: fmt.Sprintf("cmd-%d", cmdSeq), Name: name, Origin: OriginKeypad}
	for _, m := range mutate {
		m(&c)
	}
	return h.e.Handle(c)
}

func goodCode(user string) func(*Command) {
	return func(c *Command) { c.Auth = Auth{Checked: true, OK: true, User: user} }
}
func badCode(c *Command) { c.Auth = Auth{Checked: true, OK: false} }
func force(c *Command)   { c.Force = true }

func (h *harness) arm(mutate ...func(*Command)) []Action { return h.cmd(CmdArm, mutate...) }

// armed brings the engine to the armed state with no open zone.
func (h *harness) armed() {
	h.t.Helper()
	h.arm()
	h.advance(h.cfg.ExitDelay)
	h.mustBe(ModeArmed)
}

func (h *harness) mustBe(m Mode) {
	h.t.Helper()
	if got := h.e.Mode(); got != m {
		h.t.Fatalf("mode %q, want %q", got, m)
	}
}

// --- extracting from actions ------------------------------------------------

// eventsOf returns the domain events of type T among the actions.
func eventsOf[T Event](as []Action) []T {
	var out []T
	for _, a := range as {
		if e, ok := a.(Emit); ok {
			if v, ok := e.Event.(T); ok {
				out = append(out, v)
			}
		}
	}
	return out
}

// onlyEvent expects exactly one event of type T and returns it.
func onlyEvent[T Event](t *testing.T, as []Action) T {
	t.Helper()
	got := eventsOf[T](as)
	if len(got) != 1 {
		var zero T
		t.Fatalf("want 1 event %T, got %d in %v", zero, len(got), as)
	}
	return got[0]
}

func alerts(as []Action) []Alert {
	var out []Alert
	for _, a := range as {
		if x, ok := a.(Alert); ok {
			out = append(out, x)
		}
	}
	return out
}

func count[T Action](as []Action) int {
	n := 0
	for _, a := range as {
		if _, ok := a.(T); ok {
			n++
		}
	}
	return n
}

func replyOf(t *testing.T, as []Action) Reply {
	t.Helper()
	for _, a := range as {
		if r, ok := a.(Reply); ok {
			return r
		}
	}
	t.Fatalf("no reply in %v", as)
	return Reply{}
}

func noActions(t *testing.T, as []Action) {
	t.Helper()
	if len(as) != 0 {
		t.Fatalf("want no action, got %v", as)
	}
}

// --- published state -----------------------------------------------------

func TestState(t *testing.T) {
	h := newHarness(t)
	if s := h.e.State(h.now); s.Mode != ModeDisarmed || !s.EndsAt.IsZero() || s.Cycle != 0 {
		t.Fatalf("%+v", s)
	}
	h.arm()
	s := h.e.State(h.now)
	if s.Mode != ModeArming || !s.EndsAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("%+v", s)
	}
	h.advance(2 * time.Minute)
	if s := h.e.State(h.now); s.Mode != ModeArmed || !s.EndsAt.IsZero() {
		t.Fatalf("%+v", s)
	}
	h.breakWindow()
	if s := h.e.State(h.now); s.Mode != ModeTriggered || s.Cycle != 1 || s.Of != 3 {
		t.Fatalf("%+v", s)
	}
}

// Every state says the exit delay an arming would take now, the configuration's own,
// so that the app can say it before arming.
func TestStateSaysTheExitDelayInEveryMode(t *testing.T) {
	for _, delay := range []time.Duration{DefaultConfig().ExitDelay, 45 * time.Second} {
		h := newHarness(t, func(c *Config) { c.ExitDelay = delay })
		check := func(want Mode) {
			t.Helper()
			if s := h.e.State(h.now); s.Mode != want || s.ExitDelay != delay {
				t.Fatalf("%v: %+v, want %v in %s", delay, s, delay, want)
			}
		}
		check(ModeDisarmed)
		h.arm()
		check(ModeArming)
		h.advance(delay)
		check(ModeArmed)
		h.breakWindow()
		check(ModeTriggered)
	}
}

// sirenAt is where the published state of a triggered alarm says the siren schedule
// stands: cycle, in a cycle or in the pause, and when that ends (since the alarm).
type sirenAt struct {
	cycle  int
	on     bool
	endsAt time.Duration
}

// mustRing checks that the state names the incident and stands where want says.
func (h *harness) mustRing(inc string, start time.Time, want sirenAt) {
	h.t.Helper()
	s := h.e.State(h.now)
	if s.Mode != ModeTriggered || s.Incident != inc || s.Cycle != want.cycle || s.Of != h.cfg.SirenCycles ||
		s.SirenOn != want.on || !s.EndsAt.Equal(start.Add(want.endsAt)) {
		h.t.Fatalf("%v after the alarm, want %+v of %s: %+v", h.now.Sub(start), want, inc, s)
	}
}

// While triggered, the published state names the incident that is ringing, says
// whether the siren is in a cycle or in the pause between two, and until when: the
// schedule's own times, which a late tick does not move.
func TestStateFollowsTheSirenSchedule(t *testing.T) {
	h := newHarness(t)
	h.armed()
	start := h.now
	inc := onlyEvent[Triggered](t, h.breakWindow()).Incident
	for _, s := range []struct {
		after time.Duration // since the alarm
		want  sirenAt
	}{
		{0, sirenAt{1, true, 5 * time.Minute}},
		{4 * time.Minute, sirenAt{1, true, 5 * time.Minute}},
		{5*time.Minute + 30*time.Second, sirenAt{1, false, 7 * time.Minute}}, // a late tick: the pause still ends at 7
		{7 * time.Minute, sirenAt{2, true, 12 * time.Minute}},
		{12 * time.Minute, sirenAt{2, false, 14 * time.Minute}},
		{14 * time.Minute, sirenAt{3, true, 19 * time.Minute}}, // the last cycle
	} {
		h.now = start.Add(s.after)
		h.e.Handle(Tick{Now: h.now})
		h.mustRing(inc, start, s.want)
	}

	// The last cycle over, so is the incident: the state says nothing of it any more.
	h.advance(5 * time.Minute)
	if s := h.e.State(h.now); s.Mode != ModeArmed || s.Incident != "" || s.SirenOn || s.Cycle != 0 || !s.EndsAt.IsZero() {
		t.Fatalf("after the last cycle: %+v", s)
	}
}

// A person ending the alarm ends what the state says of it, in the pause too.
func TestStateForgetsTheSirenOnceTheAlarmIsStopped(t *testing.T) {
	for _, name := range []CommandName{CmdDisarm, CmdSilence} {
		h := newHarness(t)
		h.armed()
		h.breakWindow()
		h.advance(6 * time.Minute) // in the pause
		h.cmd(name, goodCode("u_owner"))
		if s := h.e.State(h.now); s.Mode == ModeTriggered || s.Incident != "" || s.SirenOn || s.Cycle != 0 || !s.EndsAt.IsZero() {
			t.Fatalf("%s: %+v", name, s)
		}
	}
}

// A fire during an intrusion alerts under an incident of its own, but the siren
// keeps the intrusion's schedule: the state still names the intrusion, on the same
// times, in a cycle as in the pause.
func TestAFireDuringAnAlarmLeavesTheStateToThatAlarm(t *testing.T) {
	for _, c := range []struct {
		name  string
		after time.Duration // since the intrusion
		want  sirenAt
	}{
		{"in a cycle", 2 * time.Minute, sirenAt{1, true, 5 * time.Minute}},
		{"in the pause", 6 * time.Minute, sirenAt{1, false, 7 * time.Minute}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.armed()
			start := h.now
			intrusion := onlyEvent[Triggered](t, h.breakWindow()).Incident
			h.advance(c.after)
			fire := alerts(h.signal(smoke, SensorSmoke, true))
			if len(fire) != 1 || fire[0].Kind != AlertFire || fire[0].Incident == intrusion {
				t.Fatalf("the fire must alert under its own incident: %+v", fire)
			}
			h.mustRing(intrusion, start, c.want)
		})
	}
}

// A fire while disarmed rings, then goes back to disarmed once its cycles ran out:
// the state then says only the next automatic arming.
func TestStateAfterAFireWhileDisarmedSaysOnlyTheNextArming(t *testing.T) {
	h := newHarness(t, closes22)
	start := h.now
	fire := onlyEvent[Triggered](t, h.signal(smoke, SensorSmoke, true)).Incident
	h.mustRing(fire, start, sirenAt{1, true, 5 * time.Minute})
	h.advance(19 * time.Minute) // three cycles and two pauses
	s := h.e.State(h.now)
	if s.Mode != ModeDisarmed || s.Incident != "" || s.SirenOn || s.Cycle != 0 || s.Of != 0 || !s.EndsAt.IsZero() ||
		!s.NextScheduled.Equal(closing) {
		t.Fatalf("after the fire's cycles: %+v", s)
	}
}

// --- configuration ---------------------------------------------------------

func TestConfigValidation(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Config){
		"zero exit delay":   func(c *Config) { c.ExitDelay = 0 },
		"negative entry":    func(c *Config) { c.EntryDelay = -time.Second },
		"zero siren cycle":  func(c *Config) { c.SirenCycle = 0 },
		"zero pause":        func(c *Config) { c.SirenPause = 0 },
		"no siren cycles":   func(c *Config) { c.SirenCycles = 0 },
		"zero lockout":      func(c *Config) { c.LockoutDuration = 0 },
		"no lockout tries":  func(c *Config) { c.LockoutAttempts = 0 },
		"zero notice":       func(c *Config) { c.Closing = allWeek(22, 0); c.AutoArmNotice = 0 },
		"zero step":         func(c *Config) { c.Closing = allWeek(22, 0); c.PostponeStep = 0 },
		"negative postpone": func(c *Config) { c.Closing = allWeek(22, 0); c.PostponeMax = -1 },
		"negative limit":    func(c *Config) { c.Closing = allWeek(22, 0); c.AutoArmLimit = -time.Hour },
		"hour 24":           func(c *Config) { c.Closing = allWeek(24, 0) },
		"minute 60":         func(c *Config) { c.Closing = allWeek(22, 60) },
		"bad weekday":       func(c *Config) { c.Closing = map[time.Weekday]TimeOfDay{7: {22, 0}} },
		"unknown zone kind": func(c *Config) { c.Zones = map[string]Zone{"d": {Name: "x", Kind: "garage"}} },
	}
	for name, mutate := range bad {
		c := DefaultConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if _, err := New(c, func() string { return "x" }, t0); err == nil {
			t.Errorf("%s: New must refuse", name)
		}
	}
	if _, err := New(DefaultConfig(), nil, t0); err == nil {
		t.Error("newID is required")
	}
}

func TestDefaultsMatchSpecification(t *testing.T) {
	c := DefaultConfig()
	if c.ExitDelay != 2*time.Minute || c.EntryDelay != 45*time.Second ||
		c.SirenCycle != 5*time.Minute || c.SirenPause != 2*time.Minute || c.SirenCycles != 3 ||
		c.LockoutAttempts != 3 || c.LockoutDuration != 10*time.Minute || c.PanicSiren ||
		c.AutoArmNotice != 10*time.Minute || c.PostponeStep != 30*time.Minute || c.PostponeMax != 3 ||
		len(c.Closing) != 0 {
		t.Fatalf("defaults differ from docs/engine.md: %+v", c)
	}
}
