package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"safehome/internal/engine"
)

var t0 = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC) // Saturday 21:00 UTC

// --- test doubles ----------------------------------------------------------------

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*fakeTimer]bool
}

type fakeTimer struct {
	when time.Time
	c    chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: t0, timers: map[*fakeTimer]bool{}} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{when: c.now.Add(d), c: make(chan time.Time, 1)}
	if d <= 0 {
		t.c <- c.now
		return t.c, func() {}
	}
	c.timers[t] = true
	return t.c, func() { c.mu.Lock(); delete(c.timers, t); c.mu.Unlock() }
}

// Advance moves time forward and fires the timers that are due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for t := range c.timers {
		if !t.when.After(c.now) {
			t.c <- c.now
			delete(c.timers, t)
		}
	}
}

// Set moves the clock to any time, including backwards.
func (c *fakeClock) Set(t time.Time) { c.mu.Lock(); c.now = t; c.mu.Unlock() }

type memStore struct {
	mu      sync.Mutex
	docs    map[string][]byte
	saves   int
	failing int // the next n saves fail
	loadErr error
	log     *[]string // shared journal, to check the order of effects
}

func newMemStore() *memStore { return &memStore{docs: map[string][]byte{}} }

func (s *memStore) Save(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing > 0 {
		s.failing--
		return errors.New("disk full")
	}
	s.docs[name] = append([]byte(nil), data...)
	s.saves++
	if s.log != nil {
		*s.log = append(*s.log, "save")
	}
	return nil
}

func (s *memStore) Load(name string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, false, s.loadErr
	}
	d, ok := s.docs[name]
	return d, ok, nil
}

func (s *memStore) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.saves }

type rig struct {
	t      *testing.T
	clock  *fakeClock
	store  *memStore
	eng    *engine.Engine
	loop   *Loop[engine.Input, engine.Action, engine.Snapshot]
	cancel context.CancelFunc
	ctx    context.Context
	done   chan error
	result error
	once   sync.Once

	mu      sync.Mutex
	actions []engine.Action
	journal []string
	dispErr error
}

func (r *rig) dispatched() []engine.Action {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]engine.Action(nil), r.actions...)
}

// engineCore adapts the alarm engine to the loop.
func engineCore(e *engine.Engine) Core[engine.Input, engine.Action, engine.Snapshot] {
	return Core[engine.Input, engine.Action, engine.Snapshot]{
		Handle:   e.Handle,
		NextWake: e.NextWake,
		Snapshot: e.Snapshot,
		Restore:  e.Restore,
		Tick:     func(now time.Time) engine.Input { return engine.Tick{Now: now} },
	}
}

func testEngineConfig() engine.Config {
	c := engine.DefaultConfig()
	c.Zones = map[string]engine.Zone{
		"dev_door": {Name: "entrance", Kind: engine.ZoneEntry},
		"dev_back": {Name: "back", Kind: engine.ZoneInstant},
	}
	return c
}

func newRig(t *testing.T, store *memStore, mutate ...func(*engine.Config)) *rig {
	t.Helper()
	r := &rig{t: t, clock: newFakeClock(), store: store}
	if store == nil {
		r.store = newMemStore()
	}
	r.store.log = &r.journal
	cfg := testEngineConfig()
	for _, m := range mutate {
		m(&cfg)
	}
	n := 0
	e, err := engine.New(cfg, func() string { n++; return fmt.Sprintf("inc-%d", n) }, t0)
	if err != nil {
		t.Fatal(err)
	}
	r.eng = e
	loop, err := New(Options[engine.Input, engine.Action, engine.Snapshot]{
		Name: "engine", Core: engineCore(e), Clock: r.clock, Store: r.store,
		Dispatch: func(_ context.Context, now time.Time, as []engine.Action) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.actions = append(r.actions, as...)
			r.journal = append(r.journal, "dispatch")
			return r.dispErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.loop = loop
	return r
}

func (r *rig) start() {
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.done = make(chan error, 1)
	go func() { r.done <- r.loop.Run(r.ctx) }()
	r.t.Cleanup(func() { r.stop() })
}

// stop cancels the loop and waits for it; it can be called any number of times.
func (r *rig) stop() error {
	r.once.Do(func() { r.cancel(); r.result = <-r.done })
	return r.result
}

// mode reads the engine's mode the only safe way: from the loop's own goroutine.
func (r *rig) mode() (m engine.Mode) {
	r.t.Helper()
	err := r.loop.Do(r.ctx, func(time.Time) []engine.Action { m = r.eng.Mode(); return nil })
	if err != nil {
		r.t.Fatal(err)
	}
	return m
}

func (r *rig) snapshot() (s engine.Snapshot) {
	r.t.Helper()
	err := r.loop.Do(r.ctx, func(time.Time) []engine.Action { s = r.eng.Snapshot(); return nil })
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

// settle waits until the loop has processed everything queued so far.
func (r *rig) settle() {
	r.t.Helper()
	if err := r.loop.Do(r.ctx, func(time.Time) []engine.Action { return nil }); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) eventually(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *rig) arm() {
	r.t.Helper()
	err := r.loop.Submit(r.ctx, func(now time.Time) engine.Input {
		return engine.Command{Now: now, ID: "m", Name: engine.CmdArm, Origin: engine.OriginApp}
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

func countEvents[T engine.Event](as []engine.Action) int {
	n := 0
	for _, a := range as {
		if e, ok := a.(engine.Emit); ok {
			if _, ok := e.Event.(T); ok {
				n++
			}
		}
	}
	return n
}

// --- tests -------------------------------------------------------------------------

func TestSubmitStampsTheInputWithTheClockAndDispatchesTheActions(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.clock.Advance(7 * time.Second)

	var stamped time.Time
	err := r.loop.Submit(r.ctx, func(now time.Time) engine.Input {
		stamped = now
		return engine.Command{Now: now, ID: "m", Name: engine.CmdArm, Origin: engine.OriginApp}
	})
	if err != nil {
		t.Fatal(err)
	}
	r.settle()
	if !stamped.Equal(t0.Add(7 * time.Second)) {
		t.Fatalf("stamped %v", stamped)
	}
	if got := countEvents[engine.ArmingStarted](r.dispatched()); got != 1 {
		t.Fatalf("want 1 ArmingStarted, got %d in %v", got, r.dispatched())
	}
}

func TestInputsAreProcessedInTheOrderTheyWereSubmitted(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	var order []int
	for i := 0; i < 50; i++ {
		i := i
		r.loop.Submit(r.ctx, func(now time.Time) engine.Input {
			order = append(order, i) // build runs on the loop goroutine: no race
			return engine.Tick{Now: now}
		})
	}
	r.settle()
	for i, v := range order {
		if v != i {
			t.Fatalf("order %v", order)
		}
	}
	if len(order) != 50 {
		t.Fatalf("%d inputs processed", len(order))
	}
}

func TestDoWaitsAndDispatchesItsActions(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	err := r.loop.Do(r.ctx, func(now time.Time) []engine.Action {
		return r.eng.Boot(now, []engine.Signal{{DeviceID: "dev_door", Sensor: engine.SensorContact, Active: true}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.snapshot().Open["dev_door"] {
		t.Fatal("Do must have finished before returning")
	}
}

func TestADeadlineBecomesATickAtTheRightTime(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.arm()
	r.settle()

	r.clock.Advance(119 * time.Second)
	r.settle()
	if countEvents[engine.Armed](r.dispatched()) != 0 {
		t.Fatal("armed too early")
	}

	r.clock.Advance(time.Second) // the exit delay ends
	r.eventually("the armed event", func() bool { return countEvents[engine.Armed](r.dispatched()) == 1 })
	if m := r.mode(); m != engine.ModeArmed {
		t.Fatalf("mode %s", m)
	}
}

func TestTheSchedulePlaysOutOnItsOwn(t *testing.T) {
	r := newRig(t, nil, func(c *engine.Config) {
		c.Closing = map[time.Weekday]engine.TimeOfDay{time.Saturday: {Hour: 22}}
	})
	r.start()
	r.settle()

	r.clock.Advance(50 * time.Minute) // 21:50: notice
	r.eventually("the notice", func() bool { return countEvents[engine.ArmingScheduled](r.dispatched()) == 1 })
	r.clock.Advance(10 * time.Minute) // 22:00: arming
	r.eventually("the arming", func() bool { return countEvents[engine.ArmingStarted](r.dispatched()) == 1 })
	r.clock.Advance(2 * time.Minute) // 22:02: armed
	r.eventually("armed", func() bool { return countEvents[engine.Armed](r.dispatched()) == 1 })
}

func TestStateIsSavedAfterAChange(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.arm()
	r.settle()

	var snap engine.Snapshot
	if err := json.Unmarshal(r.store.docs["engine"], &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Mode != engine.ModeArming {
		t.Fatalf("saved mode %s", snap.Mode)
	}
}

func TestAnUnchangedStateIsNotWrittenAgain(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.arm()
	r.settle()
	saves := r.store.count()
	if saves == 0 {
		t.Fatal("the change must have been saved")
	}

	// A signal from a device the engine does not know, and plain ticks: no change.
	for i := 0; i < 20; i++ {
		r.loop.Submit(r.ctx, func(now time.Time) engine.Input {
			return engine.Signal{Now: now, DeviceID: "dev_unknown", Sensor: engine.SensorContact, Active: true}
		})
	}
	r.settle()
	if r.store.count() != saves {
		t.Fatalf("%d writes, want %d: the SD card must not be worn for nothing", r.store.count(), saves)
	}
}

func TestEffectsComeBeforeTheSave(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.arm()
	r.settle()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.journal) < 2 || r.journal[0] != "dispatch" || r.journal[1] != "save" {
		t.Fatalf("journal %v", r.journal)
	}
}

func TestADispatchErrorDoesNotStopTheLoop(t *testing.T) {
	r := newRig(t, nil)
	r.dispErr = errors.New("broker down")
	r.start()
	r.arm()
	r.settle()
	r.clock.Advance(2 * time.Minute)
	r.eventually("the engine to carry on", func() bool { return r.mode() == engine.ModeArmed })
}

func TestAFailedSaveIsRetriedByTheNextTask(t *testing.T) {
	store := newMemStore()
	store.failing = 1
	r := newRig(t, store)
	r.start()
	r.arm() // this save fails: the disk is full, say
	r.settle()
	// The settle task itself changes nothing, but the last written copy is stale,
	// so it writes the state again.
	if store.count() != 1 {
		t.Fatalf("%d writes, want 1", store.count())
	}
	var snap engine.Snapshot
	json.Unmarshal(store.docs["engine"], &snap)
	if snap.Mode != engine.ModeArming {
		t.Fatalf("saved mode %s", snap.Mode)
	}

	r.clock.Advance(2 * time.Minute)
	r.eventually("the new state to be saved", func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		var s engine.Snapshot
		json.Unmarshal(store.docs["engine"], &s)
		return s.Mode == engine.ModeArmed
	})
}

func TestRestartRestoresTheStateAndTheScheduleKeepsRunning(t *testing.T) {
	first := newRig(t, nil)
	first.start()
	first.arm()
	first.settle()
	if err := first.stop(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	// Power cut, then a new process on the same disk, 90 seconds later.
	second := newRig(t, first.store)
	second.clock.Set(t0.Add(90 * time.Second))
	if second.eng.Mode() != engine.ModeArming {
		t.Fatalf("restored mode %s", second.eng.Mode())
	}
	second.start()
	if err := second.loop.Do(second.ctx, func(now time.Time) []engine.Action { return second.eng.Boot(now, nil) }); err != nil {
		t.Fatal(err)
	}
	second.clock.Advance(30 * time.Second) // the rest of the exit delay
	second.eventually("armed after the restart", func() bool { return second.mode() == engine.ModeArmed })
}

// A restart in the middle of an alarm: the hub's loop runs for a while (its boot
// window) before Boot. The deadlines missed meanwhile must wait for Boot, which
// resumes the siren once, for what is left of the cycle; a tick of the loop before it
// would send the siren a full cycle, then Boot the rest again.
func TestARestartDuringAnAlarmResumesTheSirenOnceThroughBoot(t *testing.T) {
	first := newRig(t, nil)
	first.start()
	first.arm()
	first.settle()
	first.clock.Advance(2 * time.Minute)
	first.eventually("armed", func() bool { return first.mode() == engine.ModeArmed })
	err := first.loop.Submit(first.ctx, func(now time.Time) engine.Input {
		return engine.Signal{Now: now, DeviceID: "dev_back", Sensor: engine.SensorContact, Active: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	first.eventually("the alarm", func() bool { return first.mode() == engine.ModeTriggered })
	alarmAt := t0.Add(2 * time.Minute)
	first.stop()

	// The hub was off from the alarm until 8 minutes later: cycle 1 ended at 5,
	// cycle 2 started at 7 and ends at 12.
	second := newRig(t, first.store)
	second.clock.Set(alarmAt.Add(8 * time.Minute))
	second.start()
	time.Sleep(20 * time.Millisecond) // the boot window: an overdue timer would fire now
	second.settle()
	if err := second.loop.Do(second.ctx, func(now time.Time) []engine.Action { return second.eng.Boot(now, nil) }); err != nil {
		t.Fatal(err)
	}
	var on []engine.SirenOn
	for _, a := range second.dispatched() {
		if s, ok := a.(engine.SirenOn); ok {
			on = append(on, s)
		}
	}
	if len(on) != 1 || on[0].Duration != 4*time.Minute {
		t.Fatalf("want one siren order for the 4 minutes left of cycle 2, got %v", second.dispatched())
	}
	second.clock.Advance(4 * time.Minute)
	second.eventually("the end of cycle 2", func() bool {
		as := second.dispatched()
		_, off := as[len(as)-1].(engine.SirenOff)
		return off
	})
}

func TestADamagedSavedStateStopsTheStart(t *testing.T) {
	store := newMemStore()
	store.loadErr = errors.New("filestore: document is corrupt")
	e, _ := engine.New(testEngineConfig(), func() string { return "x" }, t0)
	_, err := New(Options[engine.Input, engine.Action, engine.Snapshot]{
		Name: "engine", Core: engineCore(e), Clock: newFakeClock(), Store: store,
		Dispatch: func(context.Context, time.Time, []engine.Action) error { return nil },
	})
	if err == nil {
		t.Fatal("the application must be told, not silently start empty")
	}

	store.loadErr = nil
	store.docs["engine"] = []byte(`{"mode": 12}`) // valid JSON, wrong shape
	if _, err := New(Options[engine.Input, engine.Action, engine.Snapshot]{
		Name: "engine", Core: engineCore(e), Clock: newFakeClock(), Store: store,
		Dispatch: func(context.Context, time.Time, []engine.Action) error { return nil },
	}); err == nil {
		t.Fatal("a state of the wrong shape must be refused")
	}
}

func TestTimeNeverGoesBackward(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.clock.Advance(time.Hour)

	var seen []time.Time
	build := func(now time.Time) engine.Input {
		seen = append(seen, now)
		return engine.Tick{Now: now}
	}
	r.loop.Submit(r.ctx, build)
	r.settle()
	r.clock.Set(t0) // the system clock is corrected backwards by an hour
	r.loop.Submit(r.ctx, build)
	r.settle()
	r.clock.Set(t0.Add(2 * time.Hour))
	r.loop.Submit(r.ctx, build)
	r.settle()

	if len(seen) != 3 || !seen[1].Equal(seen[0]) || !seen[2].Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("times seen by the core: %v", seen)
	}
}

func TestRunStopsWithTheContextAndLaterCallsAreRefused(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()
	if err := r.stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}

	if err := r.loop.Submit(context.Background(), func(now time.Time) engine.Input { return engine.Tick{Now: now} }); !errors.Is(err, ErrStopped) {
		t.Fatalf("Submit after stop: %v", err)
	}
	if err := r.loop.Do(context.Background(), func(time.Time) []engine.Action { return nil }); !errors.Is(err, ErrStopped) {
		t.Fatalf("Do after stop: %v", err)
	}
}

func TestACancelledCallerDoesNotBlockForever(t *testing.T) {
	r := newRig(t, nil)
	// The loop is not running: nothing will ever process the task.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := r.loop.Do(ctx, func(time.Time) []engine.Action { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}

	// A full queue makes Submit wait, and the context frees it.
	for len(r.loop.tasks) < queueSize { // the timed-out Do above is still queued
		if err := r.loop.Submit(context.Background(), func(now time.Time) engine.Input { return engine.Tick{Now: now} }); err != nil {
			t.Fatal(err)
		}
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if err := r.loop.Submit(ctx2, func(now time.Time) engine.Input { return engine.Tick{Now: now} }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestOptionsAreChecked(t *testing.T) {
	e, _ := engine.New(testEngineConfig(), func() string { return "x" }, t0)
	good := Options[engine.Input, engine.Action, engine.Snapshot]{
		Name: "engine", Core: engineCore(e), Clock: newFakeClock(), Store: newMemStore(),
		Dispatch: func(context.Context, time.Time, []engine.Action) error { return nil },
	}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Options[engine.Input, engine.Action, engine.Snapshot]){
		"no name":     func(o *Options[engine.Input, engine.Action, engine.Snapshot]) { o.Name = "" },
		"no clock":    func(o *Options[engine.Input, engine.Action, engine.Snapshot]) { o.Clock = nil },
		"no store":    func(o *Options[engine.Input, engine.Action, engine.Snapshot]) { o.Store = nil },
		"no dispatch": func(o *Options[engine.Input, engine.Action, engine.Snapshot]) { o.Dispatch = nil },
		"no handle":   func(o *Options[engine.Input, engine.Action, engine.Snapshot]) { o.Core.Handle = nil },
		"no tick":     func(o *Options[engine.Input, engine.Action, engine.Snapshot]) { o.Core.Tick = nil },
		"no restore":  func(o *Options[engine.Input, engine.Action, engine.Snapshot]) { o.Core.Restore = nil },
	}
	for name, mutate := range bad {
		o := good
		mutate(&o)
		if _, err := New(o); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestSystemClock(t *testing.T) {
	c := SystemClock()
	if d := time.Since(c.Now()); d < 0 || d > time.Second {
		t.Fatalf("Now is off by %v", d)
	}
	fired, stop := c.Timer(5 * time.Millisecond)
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the timer never fired")
	}
	stop()
	_, stop = c.Timer(time.Hour)
	stop() // must not leak or panic
}

func TestOnChangeRunsWhenTheStateMovesAndOnlyThen(t *testing.T) {
	r := newRig(t, nil)
	var mu sync.Mutex
	changes := 0
	r.loop.o.OnChange = func(context.Context, time.Time) { mu.Lock(); changes++; mu.Unlock() }
	count := func() int { mu.Lock(); defer mu.Unlock(); return changes }
	r.start()

	r.settle()
	if count() != 0 {
		t.Fatalf("nothing moved yet, OnChange ran %d times", count())
	}
	r.arm()
	r.settle()
	if count() != 1 {
		t.Fatalf("the arming moved the state: OnChange ran %d times", count())
	}
	for i := 0; i < 10; i++ { // nothing changes
		r.loop.Submit(r.ctx, func(now time.Time) engine.Input { return engine.Tick{Now: now} })
	}
	r.settle()
	if count() != 1 {
		t.Fatalf("OnChange ran %d times for an unchanged state", count())
	}
	r.clock.Advance(2 * time.Minute) // armed
	r.eventually("the second change", func() bool { return count() == 2 })
}

func TestOnChangeIsNotRepeatedWhileASaveKeepsFailing(t *testing.T) {
	store := newMemStore()
	store.failing = 3
	r := newRig(t, store)
	var mu sync.Mutex
	changes := 0
	r.loop.o.OnChange = func(context.Context, time.Time) { mu.Lock(); changes++; mu.Unlock() }
	r.start()
	r.arm()
	r.settle()
	r.settle()
	r.settle()
	mu.Lock()
	defer mu.Unlock()
	if changes != 1 {
		t.Fatalf("one change announced %d times", changes)
	}
}

func TestOnChangeDoesNotRunForARestoredStateAtStartup(t *testing.T) {
	first := newRig(t, nil)
	first.start()
	first.arm()
	first.settle()
	first.stop()

	second := newRig(t, first.store)
	calls := 0
	second.loop.o.OnChange = func(context.Context, time.Time) { calls++ }
	second.start()
	second.settle()
	second.settle()
	if calls != 0 {
		t.Fatalf("OnChange ran %d times after a plain restart", calls)
	}
}

func TestPostRunsWithoutWaiting(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	done := make(chan struct{})
	if err := r.loop.Post(context.Background(), func(time.Time) []engine.Action { close(done); return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the posted function never ran")
	}
}

func TestAliveSaysWhetherTheLoopStillAnswers(t *testing.T) {
	r := newRig(t, nil)
	// Not running: as stuck as a loop can be.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.loop.Alive(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	r.start()
	r.settle()
	saves := r.store.count()
	for range 5 {
		if err := r.loop.Alive(r.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if r.store.count() != saves {
		t.Fatal("Alive must not touch the core nor the disk")
	}
	// Stuck in the core: Alive waits, then gives up.
	release := make(chan struct{})
	r.loop.Post(r.ctx, func(time.Time) []engine.Action { <-release; return nil })
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if err := r.loop.Alive(ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	close(release)
	if err := r.loop.Alive(r.ctx); err != nil {
		t.Fatal(err)
	}
	r.stop()
	if err := r.loop.Alive(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("got %v", err)
	}
}

func TestEveryRepeatsUntilItsContextEnds(t *testing.T) {
	clock := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	runs := make(chan time.Time, 10)
	done := make(chan struct{})
	go func() {
		Every(ctx, clock, time.Minute, func(context.Context) { runs <- clock.Now() })
		close(done)
	}()
	for i := 1; i <= 2; i++ {
		waitForTimer(t, clock)
		clock.Advance(time.Minute)
		if at := <-runs; !at.Equal(t0.Add(time.Duration(i) * time.Minute)) {
			t.Fatalf("run %d at %v", i, at)
		}
	}
	cancel()
	<-done
	if len(runs) != 0 {
		t.Fatal("no run after the end")
	}
}

// waitForTimer waits until a timer is set on the clock.
func waitForTimer(t *testing.T, c *fakeClock) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.timers)
		c.mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no timer set")
		}
		time.Sleep(time.Millisecond)
	}
}
