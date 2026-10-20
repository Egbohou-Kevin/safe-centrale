package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
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

// --- arming ------------------------------------------------------------------

func TestArmThenExitDelayElapses(t *testing.T) {
	h := newHarness(t)
	as := h.arm(func(c *Command) { c.User = "u_42" })

	h.mustBe(ModeArming)
	if r := replyOf(t, as); r.Status != StatusDone {
		t.Fatalf("reply %+v", r)
	}
	ev := onlyEvent[ArmingStarted](t, as)
	if want := t0.Add(2 * time.Minute); !ev.EndsAt.Equal(want) {
		t.Fatalf("EndsAt %v, want %v", ev.EndsAt, want)
	}
	if ev.Origin != OriginKeypad || ev.User != "u_42" || ev.Forced || len(ev.Bypassed) != 0 {
		t.Fatalf("arming event: %+v", ev)
	}
	if got := h.e.NextWake(); !got.Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("NextWake %v", got)
	}

	noActions(t, h.advance(119*time.Second)) // not yet
	h.mustBe(ModeArming)

	as = h.advance(time.Second)
	h.mustBe(ModeArmed)
	armed := onlyEvent[Armed](t, as)
	if armed.Origin != OriginKeypad || armed.User != "u_42" {
		t.Fatalf("armed event: %+v", armed)
	}
	if !h.e.NextWake().IsZero() {
		t.Fatal("armed: no deadline expected")
	}
}

func TestArmNowSkipsExitDelay(t *testing.T) {
	h := newHarness(t)
	h.arm()
	h.now = h.now.Add(10 * time.Second)
	as := h.cmd(CmdArmNow, func(c *Command) { c.Origin = OriginApp })
	h.mustBe(ModeArmed)
	if got := onlyEvent[Armed](t, as); got.Origin != OriginApp {
		t.Fatalf("armed event: %+v", got)
	}
	if !h.e.NextWake().IsZero() {
		t.Fatal("arm_now must clear the deadline")
	}
	// A tick at the old deadline emits nothing more.
	noActions(t, h.advance(2*time.Minute))
}

func TestArmNowRefusedWhenDisarmed(t *testing.T) {
	h := newHarness(t)
	r := replyOf(t, h.cmd(CmdArmNow))
	if r.Status != StatusRefused || r.Reason != ReasonWrongState {
		t.Fatalf("reply %+v", r)
	}
}

func TestDisarmDuringExitDelayCancelsArming(t *testing.T) {
	h := newHarness(t)
	h.arm()
	h.advance(30 * time.Second)
	as := h.cmd(CmdDisarm, goodCode("u_42"))
	h.mustBe(ModeDisarmed)
	if got := onlyEvent[Disarmed](t, as); got.User != "u_42" {
		t.Fatalf("disarmed by %q", got.User)
	}
	noActions(t, h.advance(5*time.Minute)) // nothing else happens
}

func TestArmTwiceIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.arm()
	as := h.arm()
	if r := replyOf(t, as); r.Status != StatusDone || len(eventsOf[ArmingStarted](as)) != 0 {
		t.Fatalf("a second arm must reply done with no new event: %v", as)
	}
}

func TestEntryZonesIgnoredDuringExitDelay(t *testing.T) {
	h := newHarness(t)
	h.arm()
	noActions(t, h.open(door)) // leaving: the door opens
	noActions(t, h.close(door))
	noActions(t, h.moves())
	h.mustBe(ModeArming)
	h.advance(2 * time.Minute)
	h.mustBe(ModeArmed)
}

func TestArmRefusedWhenEntryOrPerimeterOpen(t *testing.T) {
	h := newHarness(t)
	h.open(back)
	h.open(door)
	as := h.arm()
	h.mustBe(ModeDisarmed)
	r := replyOf(t, as)
	if r.Status != StatusRefused || r.Reason != ReasonZoneOpen {
		t.Fatalf("reply %+v", r)
	}
	if got := r.Detail["devices"]; !reflect.DeepEqual(got, []string{back, door}) {
		t.Fatalf("open zones %v (want the sorted list)", got)
	}
	if len(eventsOf[ArmingStarted](as)) != 0 {
		t.Fatal("no arming event on a refusal")
	}
}

func TestOpenInteriorDoesNotBlockArming(t *testing.T) {
	h := newHarness(t)
	h.moves() // motion still active must not block arming
	h.arm()
	h.mustBe(ModeArming)
}

func TestForcedArmBypassesOpenZones(t *testing.T) {
	h := newHarness(t)
	h.open(back)
	as := h.arm(force)
	h.mustBe(ModeArming)
	ev := onlyEvent[ArmingStarted](t, as)
	if !ev.Forced || !reflect.DeepEqual(ev.Bypassed, []string{back}) {
		t.Fatalf("forced arming reported wrongly: %+v", ev)
	}
	h.advance(2 * time.Minute)

	// The bypassed zone has no effect until disarming.
	h.close(back)
	noActions(t, h.open(back))
	h.mustBe(ModeArmed)

	// The other zones still watch.
	h.breakWindow()
	h.mustBe(ModeTriggered)
}

func TestBypassClearedOnDisarm(t *testing.T) {
	h := newHarness(t)
	h.open(back)
	h.arm(force)
	h.advance(2 * time.Minute)
	h.cmd(CmdDisarm, goodCode("u_42"))
	h.close(back)

	h.armed()
	h.open(back) // no longer bypassed
	h.mustBe(ModeTriggered)
}

func TestBypassCommand(t *testing.T) {
	h := newHarness(t)
	h.open(back)
	h.cmd(CmdBypass, func(c *Command) { c.Devices = []string{back} })
	h.arm() // no need to force: the zone is already bypassed
	h.mustBe(ModeArming)

	h.advance(2 * time.Minute)
	r := replyOf(t, h.cmd(CmdBypass, func(c *Command) { c.Devices = []string{window} }))
	if r.Status != StatusRefused || r.Reason != ReasonWrongState {
		t.Fatalf("bypassing while armed must be refused: %+v", r)
	}
}

// --- entry and alarm -----------------------------------------------------

func TestEntryDelayThenDisarmInTime(t *testing.T) {
	h := newHarness(t)
	h.armed()

	as := h.open(door)
	h.mustBe(ModeEntryDelay)
	ev := onlyEvent[EntryDelayStarted](t, as)
	if ev.DeviceID != door || !ev.EndsAt.Equal(h.now.Add(45*time.Second)) {
		t.Fatalf("entry delay: %+v", ev)
	}

	h.advance(40 * time.Second)
	h.mustBe(ModeEntryDelay)
	as = h.cmd(CmdDisarm, goodCode("u_guard"))
	h.mustBe(ModeDisarmed)
	if len(alerts(as)) != 0 || count[SirenOn](as) != 0 {
		t.Fatalf("disarmed in time: no siren and no alert, got %v", as)
	}
	noActions(t, h.advance(10*time.Minute))
}

func TestEntryDelayExpiresAndTriggers(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.open(door)

	as := h.advance(45 * time.Second)
	h.mustBe(ModeTriggered)
	tr := onlyEvent[Triggered](t, as)
	if tr.Cause.DeviceID != door || tr.Cause.Sensor != SensorContact || tr.Cause.Zone != "entrance" || tr.Incident == "" {
		t.Fatalf("cause of the alarm: %+v", tr)
	}
	if count[SirenOn](as) != 1 {
		t.Fatalf("the siren must start: %v", as)
	}
	al := alerts(as)
	if len(al) != 1 || al[0].Kind != AlertIntrusion || al[0].Urgency != UrgencyHigh || al[0].Incident != tr.Incident {
		t.Fatalf("alert: %+v", al)
	}
	cy := onlyEvent[SirenCycleStarted](t, as)
	if cy.Cycle != 1 || cy.Of != 3 || cy.Incident != tr.Incident {
		t.Fatalf("cycle: %+v", cy)
	}
}

func TestLateTickStillTriggersAtDeadline(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.open(door)
	// The tick is very late: the alarm is replayed at its own time (45 s), so the
	// siren has already run a whole cycle and started its pause.
	as := h.advance(5*time.Minute + 45*time.Second + time.Second)
	if count[SirenOn](as) != 1 || count[SirenOff](as) != 1 {
		t.Fatalf("want the siren started then stopped, got %v", as)
	}
	h.mustBe(ModeTriggered)
	if got := h.e.Snapshot().Incident.Cycle; got != 1 {
		t.Fatalf("cycle %d", got)
	}
}

func TestInstantZoneTriggersImmediately(t *testing.T) {
	for _, tc := range []struct {
		dev    string
		sensor SensorKind
	}{
		{window, SensorGlassBreak},
		{window, SensorVibration},
		{back, SensorContact},
	} {
		t.Run(tc.dev+"/"+string(tc.sensor), func(t *testing.T) {
			h := newHarness(t)
			h.armed()
			as := h.signal(tc.dev, tc.sensor, true)
			h.mustBe(ModeTriggered)
			if count[SirenOn](as) != 1 || len(alerts(as)) != 1 {
				t.Fatalf("want siren and alert: %v", as)
			}
		})
	}
}

func TestInteriorMotionTriggersWhenArmed(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.moves()
	h.mustBe(ModeTriggered)
}

func TestMotionIgnoredDuringEntryDelay(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.open(door)
	noActions(t, h.moves()) // the person coming in walks to the keypad
	h.mustBe(ModeEntryDelay)
}

func TestSecondDoorOpeningDoesNotRestartEntryDelay(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.open(door)
	h.advance(30 * time.Second)
	h.close(door)
	noActions(t, h.open(door))
	h.advance(15 * time.Second) // 45 s since the first opening
	h.mustBe(ModeTriggered)
}

func TestInstantZoneDuringEntryDelayTriggers(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.open(door)
	h.breakWindow()
	h.mustBe(ModeTriggered)
	if h.e.Snapshot().EntryCause != nil {
		t.Fatal("the entry cause must be cleared")
	}
	noActions(t, h.advance(0))
}

func TestNothingWatchesWhenDisarmed(t *testing.T) {
	h := newHarness(t)
	noActions(t, h.open(door))
	noActions(t, h.open(back))
	noActions(t, h.moves())
	noActions(t, h.breakWindow())
	h.mustBe(ModeDisarmed)
}

func TestUnknownDeviceAndInactiveSignalsAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.armed()
	noActions(t, h.signal("dev_unknown", SensorContact, true))
	noActions(t, h.close(door))
	noActions(t, h.signal(motion, SensorMotion, false))
	h.mustBe(ModeArmed)
}

// --- siren: cycles, resumption, end -------------------------------------------

func TestSirenCyclesThenBackToArmed(t *testing.T) {
	h := newHarness(t)
	h.armed()
	start := h.now
	as := h.breakWindow()
	inc := onlyEvent[Triggered](t, as).Incident

	type step struct {
		after time.Duration // since the alarm
		on    int
		off   int
		cycle int // cycle number announced, 0 if none
	}
	// 5 min of siren, 2 min pause, three cycles.
	steps := []step{
		{5 * time.Minute, 0, 1, 0},  // end of cycle 1
		{7 * time.Minute, 1, 0, 2},  // start of cycle 2
		{12 * time.Minute, 0, 1, 0}, // end of cycle 2
		{14 * time.Minute, 1, 0, 3}, // start of cycle 3
		{19 * time.Minute, 0, 1, 0}, // end of cycle 3: incident over
	}
	for i, s := range steps {
		h.now = start.Add(s.after - time.Second)
		noActions(t, h.e.Handle(Tick{Now: h.now})) // one second before: nothing
		h.now = start.Add(s.after)
		as := h.e.Handle(Tick{Now: h.now})
		if count[SirenOn](as) != s.on || count[SirenOff](as) != s.off {
			t.Fatalf("step %d (%v): siren on=%d off=%d, got %v", i, s.after, s.on, s.off, as)
		}
		if s.cycle > 0 {
			cy := onlyEvent[SirenCycleStarted](t, as)
			if cy.Cycle != s.cycle || cy.Incident != inc {
				t.Fatalf("step %d: cycle %+v", i, cy)
			}
		}
	}

	h.mustBe(ModeArmed)
	snap := h.e.Snapshot()
	if snap.Incident != nil || !snap.Deadline.IsZero() {
		t.Fatalf("the incident must be closed: %+v", snap)
	}
	if snap.LastTrigger == nil || snap.LastTrigger.DeviceID != window {
		t.Fatalf("memory of the alarm lost: %+v", snap.LastTrigger)
	}

	// Armed again: a new signal makes a new incident.
	as = h.moves()
	if got := onlyEvent[Triggered](t, as).Incident; got == inc {
		t.Fatal("a new incident needs a new identifier")
	}
}

func TestClearedByTimeout(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	var cleared []Cleared
	for _, d := range []time.Duration{5 * time.Minute, 2 * time.Minute, 5 * time.Minute, 2 * time.Minute, 5 * time.Minute} {
		cleared = append(cleared, eventsOf[Cleared](h.advance(d))...)
	}
	if len(cleared) != 1 || cleared[0].By != ClearedByTimeout || cleared[0].Duration != 19*time.Minute {
		t.Fatalf("cleared: %+v", cleared)
	}
}

func TestDisarmWhileTriggeredStopsEverything(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	h.advance(90 * time.Second)

	as := h.cmd(CmdDisarm, goodCode("u_owner"), func(c *Command) { c.Origin = OriginApp })
	h.mustBe(ModeDisarmed)
	if count[SirenOff](as) != 1 {
		t.Fatalf("the siren must stop: %v", as)
	}
	cl := onlyEvent[Cleared](t, as)
	if cl.By != ClearedByUser || cl.User != "u_owner" || cl.Duration != 90*time.Second {
		t.Fatalf("cleared: %+v", cl)
	}
	onlyEvent[Disarmed](t, as)
	noActions(t, h.advance(time.Hour)) // no cycle resumes
}

func TestSilenceKeepsSystemArmed(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()

	as := h.cmd(CmdSilence, goodCode("u_guard"))
	h.mustBe(ModeArmed)
	if count[SirenOff](as) != 1 || len(eventsOf[Cleared](as)) != 1 || len(eventsOf[Disarmed](as)) != 0 {
		t.Fatalf("silence: %v", as)
	}
	noActions(t, h.advance(time.Hour))

	// The zones still watch.
	h.moves()
	h.mustBe(ModeTriggered)
}

func TestSilenceRefusedWhenNotTriggered(t *testing.T) {
	h := newHarness(t)
	h.armed()
	r := replyOf(t, h.cmd(CmdSilence, goodCode("u_guard")))
	if r.Status != StatusRefused || r.Reason != ReasonWrongState {
		t.Fatalf("reply %+v", r)
	}
}

// --- permanent zones ------------------------------------------------------

func TestSmokeTriggersInEveryMode(t *testing.T) {
	for _, mode := range []string{"disarmed", "arming", "armed"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			switch mode {
			case "arming":
				h.arm()
			case "armed":
				h.armed()
			}
			as := h.signal(smoke, SensorSmoke, true)
			h.mustBe(ModeTriggered)
			al := alerts(as)
			if len(al) != 1 || al[0].Kind != AlertFire || al[0].Urgency != UrgencyCritical {
				t.Fatalf("fire alert: %+v", al)
			}
			if count[SirenOn](as) != 1 {
				t.Fatal("the siren must sound")
			}
		})
	}
}

func TestSmokeWhileDisarmedReturnsToDisarmed(t *testing.T) {
	h := newHarness(t)
	h.signal(smoke, SensorSmoke, true)
	for i := 0; i < 5; i++ {
		h.advance(7 * time.Minute)
	}
	h.mustBe(ModeDisarmed)
	// And an opening door does nothing: the system is not armed.
	noActions(t, h.open(door))
}

func TestSmokeDuringIntrusionAlertsToo(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	as := h.signal(smoke, SensorSmoke, true)
	al := alerts(as)
	if len(al) != 1 || al[0].Kind != AlertFire {
		t.Fatalf("a fire during an intrusion must alert: %v", as)
	}
	if count[SirenOn](as) != 0 {
		t.Fatal("the siren is already ringing")
	}
}

func TestSecondIntrusionSignalDuringAlarmDoesNothing(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	noActions(t, h.moves())
}

func TestPanicIsSilentByDefault(t *testing.T) {
	for _, mode := range []string{"disarmed", "armed"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			if mode == "armed" {
				h.armed()
			}
			before := h.e.Mode()
			as := h.signal(panic_, SensorPanic, true)
			h.mustBe(before)
			al := alerts(as)
			if len(al) != 1 || al[0].Kind != AlertPanic || al[0].Urgency != UrgencyCritical {
				t.Fatalf("panic alert: %+v", al)
			}
			if count[SirenOn](as) != 0 {
				t.Fatal("a silent panic does not sound the siren")
			}
		})
	}
}

func TestPanicWithSirenOption(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.PanicSiren = true })
	as := h.signal(panic_, SensorPanic, true)
	h.mustBe(ModeTriggered)
	if count[SirenOn](as) != 1 || alerts(as)[0].Kind != AlertPanic {
		t.Fatalf("panic with siren: %v", as)
	}
}

func TestPanicDuringIntrusionAlerts(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	as := h.signal(panic_, SensorPanic, true)
	if al := alerts(as); len(al) != 1 || al[0].Kind != AlertPanic {
		t.Fatalf("the panic must alert: %v", as)
	}
}

func TestTamperDisarmedAlertsWithoutSiren(t *testing.T) {
	h := newHarness(t)
	as := h.signal(tamper, SensorTamper, true)
	h.mustBe(ModeDisarmed)
	if al := alerts(as); len(al) != 1 || al[0].Kind != AlertTamper || count[SirenOn](as) != 0 {
		t.Fatalf("tamper while disarmed: %v", as)
	}
}

func TestTamperArmedTriggers(t *testing.T) {
	h := newHarness(t)
	h.armed()
	as := h.signal(tamper, SensorTamper, true)
	h.mustBe(ModeTriggered)
	if al := alerts(as); len(al) != 1 || al[0].Kind != AlertTamper || count[SirenOn](as) != 1 {
		t.Fatalf("tamper while armed: %v", as)
	}
}

func TestPermanentZonesCannotBeBypassed(t *testing.T) {
	h := newHarness(t)
	h.cmd(CmdBypass, func(c *Command) { c.Devices = []string{smoke, tamper} })
	h.signal(smoke, SensorSmoke, true)
	h.mustBe(ModeTriggered)
}

// --- codes, lockout ---------------------------------------------------------

func TestBadCodesLockOutThenRecover(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()

	// 1st and 2nd wrong codes: refusal and event, no lockout yet.
	for i := 1; i <= 2; i++ {
		as := h.cmd(CmdDisarm, badCode)
		r := replyOf(t, as)
		if r.Status != StatusRefused || r.Reason != ReasonBadCode {
			t.Fatalf("attempt %d: %+v", i, r)
		}
		af := onlyEvent[AuthFailed](t, as)
		if af.Attempts != i || !af.LockedUntil.IsZero() || len(alerts(as)) != 0 {
			t.Fatalf("attempt %d: %+v alerts=%v", i, af, alerts(as))
		}
		h.mustBe(ModeTriggered)
	}

	// 3rd wrong code: locked for 10 minutes, with an alert.
	as := h.cmd(CmdDisarm, badCode)
	af := onlyEvent[AuthFailed](t, as)
	if af.Attempts != 3 || !af.LockedUntil.Equal(h.now.Add(10*time.Minute)) {
		t.Fatalf("lockout: %+v", af)
	}
	if al := alerts(as); len(al) != 1 || al[0].Kind != AlertAuthFailed {
		t.Fatalf("lockout alert: %v", al)
	}
	if got := h.e.State(h.now).LockedUntil; got.IsZero() {
		t.Fatal("State must announce the lockout")
	}

	// During the lockout, even the right code is refused.
	h.advance(9 * time.Minute)
	r := replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner")))
	if r.Status != StatusRefused || r.Reason != ReasonLockedOut {
		t.Fatalf("during the lockout: %+v", r)
	}
	h.mustBe(ModeTriggered)
	// And a wrong code during the lockout does not count.
	if got := h.e.Snapshot().Failed; got != 0 {
		t.Fatalf("counter %d", got)
	}
	r = replyOf(t, h.cmd(CmdDisarm, badCode))
	if r.Reason != ReasonLockedOut {
		t.Fatalf("wrong code during the lockout: %+v", r)
	}

	// After the 10 minutes, the right code works.
	h.advance(time.Minute)
	if got := h.e.State(h.now).LockedUntil; !got.IsZero() {
		t.Fatal("the lockout is over")
	}
	r = replyOf(t, h.cmd(CmdDisarm, goodCode("u_owner")))
	if r.Status != StatusDone {
		t.Fatalf("after the lockout: %+v", r)
	}
	h.mustBe(ModeDisarmed)
}

func TestGoodCodeResetsFailureCounter(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.cmd(CmdDisarm, badCode)
	h.cmd(CmdDisarm, badCode)
	h.cmd(CmdDisarm, goodCode("u_42"))
	h.mustBe(ModeDisarmed)
	h.armed()
	as := h.cmd(CmdDisarm, badCode)
	if af := onlyEvent[AuthFailed](t, as); af.Attempts != 1 {
		t.Fatalf("the counter must start over: %+v", af)
	}
}

func TestUncheckedCodeCountsAsBadCode(t *testing.T) {
	h := newHarness(t)
	h.armed()
	r := replyOf(t, h.cmd(CmdDisarm)) // the driver checked no code
	if r.Status != StatusRefused || r.Reason != ReasonBadCode {
		t.Fatalf("a disarm with no checked code must be refused: %+v", r)
	}
	h.mustBe(ModeArmed)
}

func TestArmNeedsNoCodeAndIgnoresLockout(t *testing.T) {
	h := newHarness(t)
	h.cmd(CmdDisarm, badCode)
	h.cmd(CmdDisarm, badCode)
	h.cmd(CmdDisarm, badCode) // locked
	r := replyOf(t, h.arm())
	if r.Status != StatusDone {
		t.Fatalf("arming stays allowed during the lockout: %+v", r)
	}
}

func TestDisarmWhenAlreadyDisarmedIsIdempotent(t *testing.T) {
	h := newHarness(t)
	as := h.cmd(CmdDisarm, goodCode("u_42"))
	if r := replyOf(t, as); r.Status != StatusDone || len(eventsOf[Disarmed](as)) != 0 {
		t.Fatalf("%v", as)
	}
}

// --- orders: expiry, unknown ------------------------------------------------

func TestExpiredCommandIsNotExecuted(t *testing.T) {
	h := newHarness(t)
	as := h.arm(func(c *Command) { c.Exp = h.now.Add(-time.Second) })
	h.mustBe(ModeDisarmed)
	if r := replyOf(t, as); r.Status != StatusExpired {
		t.Fatalf("reply %+v", r)
	}
	// Exactly at the expiry time, the order still goes through.
	h.arm(func(c *Command) { c.Exp = h.now })
	h.mustBe(ModeArming)
}

func TestUnknownCommand(t *testing.T) {
	h := newHarness(t)
	r := replyOf(t, h.cmd("explode"))
	if r.Status != StatusFailed || r.Reason != ReasonUnknownCommand {
		t.Errorf("%+v", r)
	}
}

func TestRepliesCarryCommandID(t *testing.T) {
	h := newHarness(t)
	as := h.e.Handle(Command{Now: h.now, ID: "my-order", Name: CmdArm, Origin: OriginApp})
	if r := replyOf(t, as); r.Ref != "my-order" {
		t.Fatalf("ref %q", r.Ref)
	}
}

func TestArmRefusedDuringEntryDelayAndAlarm(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.open(door)
	if r := replyOf(t, h.arm()); r.Reason != ReasonWrongState {
		t.Fatalf("%+v", r)
	}
	h.advance(45 * time.Second)
	if r := replyOf(t, h.arm()); r.Reason != ReasonWrongState {
		t.Fatalf("%+v", r)
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

// --- persistence and restart ------------------------------------------------

func restart(t *testing.T, h *harness) *Engine {
	t.Helper()
	b, err := json.Marshal(h.e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	e, err := New(h.cfg, func() string { h.ids++; return fmt.Sprintf("inc-%d", h.ids) }, h.now)
	if err != nil {
		t.Fatal(err)
	}
	e.Restore(snap)
	return e
}

func TestRestartWhileArmedStaysArmed(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.now = h.now.Add(time.Minute)
	h.e = restart(t, h)
	noActions(t, h.e.Boot(h.now, nil))
	h.mustBe(ModeArmed)
	h.open(back)
	h.mustBe(ModeTriggered)
}

func TestRestartWhileArmingKeepsRemainingDelay(t *testing.T) {
	h := newHarness(t)
	h.arm()
	h.now = h.now.Add(30 * time.Second)
	h.e = restart(t, h)
	noActions(t, h.e.Boot(h.now, nil))
	h.mustBe(ModeArming)
	h.advance(90 * time.Second) // 2 min since arming
	h.mustBe(ModeArmed)
}

func TestRestartAfterDelayElapsedFinishesArming(t *testing.T) {
	h := newHarness(t)
	h.arm()
	restarted := restart(t, h)
	h.now = h.now.Add(10 * time.Minute) // the hub stayed off
	h.e = restarted
	as := h.e.Boot(h.now, nil)
	h.mustBe(ModeArmed)
	onlyEvent[Armed](t, as)
}

func TestRestartWhileTriggeredResumesSiren(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	h.now = h.now.Add(time.Minute)
	h.e = restart(t, h)

	as := h.e.Boot(h.now, nil)
	h.mustBe(ModeTriggered)
	var on []SirenOn
	for _, a := range as {
		if s, ok := a.(SirenOn); ok {
			on = append(on, s)
		}
	}
	if len(on) != 1 || on[0].Duration != 4*time.Minute {
		t.Fatalf("the siren must resume for the 4 remaining minutes: %v", as)
	}
	// The schedule goes on: the end of cycle 1 is 5 minutes after the alarm.
	if count[SirenOff](h.advance(4*time.Minute)) != 1 {
		t.Fatal("end of cycle expected")
	}
}

func TestRestartDuringSirenPauseDoesNotResumeSiren(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	h.advance(6 * time.Minute) // pause between cycles 1 and 2
	h.e = restart(t, h)
	if count[SirenOn](h.e.Boot(h.now, nil)) != 0 {
		t.Fatal("no siren during the pause")
	}
}

func TestBootTriggersOnZonesAlreadyInAlarm(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.e = restart(t, h)
	as := h.e.Boot(h.now, []Signal{
		{DeviceID: back, Sensor: SensorContact, Active: true},
		{DeviceID: door, Sensor: SensorContact, Active: false},
	})
	h.mustBe(ModeTriggered)
	if count[SirenOn](as) != 1 || len(alerts(as)) != 1 {
		t.Fatalf("a back door open at restart must trigger: %v", as)
	}
}

func TestBootDisarmedLearnsOpenDoors(t *testing.T) {
	h := newHarness(t)
	h.e = restart(t, h)
	noActions(t, h.e.Boot(h.now, []Signal{{DeviceID: back, Sensor: SensorContact, Active: true}}))
	r := replyOf(t, h.arm())
	if r.Reason != ReasonZoneOpen {
		t.Fatalf("a door known open at boot must block arming: %+v", r)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	h := newHarness(t)
	h.open(back)
	snap := h.e.Snapshot()
	snap.Open["dev_ghost"] = true
	snap.Mode = ModeTriggered
	if h.e.Mode() != ModeDisarmed || h.e.Snapshot().Open["dev_ghost"] {
		t.Fatal("changing a copy must not change the engine")
	}
}

func TestSnapshotJSONRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.open(back)
	h.arm(force) // the back door is bypassed: the snapshot keeps that detail
	h.advance(h.cfg.ExitDelay)
	h.mustBe(ModeArmed)
	h.cmd(CmdDisarm, badCode)
	want := h.e.Snapshot()
	if !want.Bypassed[back] || !want.Open[back] || want.Failed != 1 {
		t.Fatalf("the snapshot must hold what we want to save: %+v", want)
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Snapshot
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON round trip:\n got %+v\nwant %+v", got, want)
	}
}
