package engine

import (
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

// Random sequences of inputs, each from a fixed seed, checked after every input
// against what must hold whatever happens: the siren follows the alarm, an incident
// goes off once, a good code disarms, a restored state goes on as the live one did, a
// clock set back neither disarms nor holds a deadline beyond its own length, a restart
// acts on nothing the engine heard, with the clock set back or once it caught up, the
// broker giving back a state older than the one heard, and every alarm that went off
// ends cleared. A failure prints the seed and the sequence, which
// replay it. The checks see their defects (TestTheRandomChecksSeeTheirDefects).

const (
	randomSeeds = 200
	randomSteps = 80
)

type sensorRef struct {
	dev    string
	sensor SensorKind
}

var randomSensors = []sensorRef{
	{door, SensorContact}, {back, SensorContact}, {window, SensorGlassBreak}, {motion, SensorMotion},
	{smoke, SensorSmoke}, {panic_, SensorPanic}, {tamper, SensorTamper}, {door, SensorTamper},
}

// rig drives one engine through a random sequence and keeps what it observed.
type rig struct {
	*harness
	states map[sensorRef]Signal // the last state of each sensor, given back at Boot
	prev   map[sensorRef]Signal // the state of each sensor before its last one
	// retained are the states the broker gave back from its last save, older than the
	// one the engine heard (a door closed after that save comes back open): given back
	// at each Boot until the sensor sends again.
	retained map[sensorRef]Signal
	// apart are the sensors heard after a time the clock went back to: the engine sets
	// their time apart until the clock catches up with it (see pullBack).
	apart map[sensorRef]bool
	// reached is the latest time the clock reached, before it was set back.
	reached time.Time
	// before and booted are the mode before the last restart, and what Boot did.
	before Mode
	booted []Action
	// caughtUp counts the restarts checked after the clock caught up with a time set
	// apart, the broker giving back a state older than it: the class the sequences must
	// reach (see TestRandomSequencesKeepTheAlarmInvariants).
	caughtUp int
	// blurred are the momentary sensors (a movement, glass breaking) whose state was
	// taken before a clock set back. The engine keeps no time of theirs, which every
	// passer-by would rewrite, and compares them with the start of the mode: once the
	// clock went back, a state dated up to the new time cannot be told from one dated
	// since, which is news (see behind), and one dated after it from a mode begun after
	// the clock went back. No fork starts while one of them is active, and a restart
	// with the clock set back is not checked then: a limit left as it is, written in
	// docs/engine.md (28 October 2026). A door, a panic button keep their time.
	blurred map[sensorRef]bool
	// unsure is set by a restart that could not be checked: a momentary sensor active,
	// blurred or, the clock set back, dated up to the new time; or a state the broker
	// kept dated up to now while the one heard is after now (see staleBehind).
	unsure    bool
	sirenOn   bool // the last siren order was SirenOn
	triggered map[string]bool
	alerted   map[string]bool
	cleared   map[string]bool
}

func newRig(t *testing.T) *rig {
	return &rig{
		harness: newHarness(t), states: map[sensorRef]Signal{}, blurred: map[sensorRef]bool{},
		prev: map[sensorRef]Signal{}, retained: map[sensorRef]Signal{}, apart: map[sensorRef]bool{},
		triggered: map[string]bool{}, alerted: map[string]bool{}, cleared: map[string]bool{},
	}
}

// fork is a rig whose engine restarts from r's saved state, Boot given the sensors as
// they are now. It returns Boot's actions; the fork observes nothing.
func (r *rig) fork() (*rig, []Action) {
	h := &harness{t: r.t, e: r.e, now: r.now, ids: r.ids, cfg: r.cfg}
	h.e = restart(r.t, h)
	f := &rig{
		harness: h, states: maps.Clone(r.states), blurred: maps.Clone(r.blurred),
		prev: maps.Clone(r.prev), retained: maps.Clone(r.retained), apart: maps.Clone(r.apart),
		reached: r.reached,
	}
	return f, f.boot()
}

func (r *rig) boot() []Action {
	var current []Signal
	for _, s := range randomSensors {
		if sig, ok := r.retained[s]; ok {
			current = append(current, sig)
		} else if sig, ok := r.states[s]; ok {
			current = append(current, sig)
		}
	}
	return r.e.Boot(r.now, current)
}

func (r *rig) blurredActive() bool {
	for s := range r.blurred {
		if r.states[s].Active {
			return true
		}
	}
	return false
}

// blur marks the momentary sensors' states as taken before a clock set back.
func (r *rig) blur() {
	for s := range r.states {
		if !lasting(s.sensor) && s.sensor != SensorPanic {
			r.blurred[s] = true
		}
	}
}

// setApart marks the sensors that keep a time and were heard after now, the clock just
// set back: the engine sets their time apart.
func (r *rig) setApart() {
	for s, sig := range r.states {
		if keepsTime(s.sensor) && sig.Now.After(r.now) {
			r.apart[s] = true
		}
	}
}

// loseLastSave gives back, for each sensor that keeps a time, the state before its last
// one when that is dated after now: the broker lost its last writes in the stop. Not
// one dated after the last, the clock set back between the two: the engine cannot
// order them, a limit written in docs/engine.md (28 October 2026).
func (r *rig) loseLastSave() {
	for s, p := range r.prev {
		if keepsTime(s.sensor) && p.Now.After(r.now) && p.Now.Before(r.states[s].Now) {
			r.retained[s] = p
		}
	}
}

// staleBehind tells whether the broker gives back a state dated up to now while the
// one the engine heard is after now: it cannot be told from a state dated while the hub
// was starting, the limit written in docs/engine.md (28 October 2026).
func (r *rig) staleBehind() bool {
	for s, sig := range r.retained {
		if !sig.Now.After(r.now) && r.states[s].Now.After(r.now) {
			return true
		}
	}
	return false
}

// staleCaughtUp tells whether the broker gives back a state older than one heard after
// a time the clock went back to, the clock since caught up with it.
func (r *rig) staleCaughtUp() bool {
	for s := range r.retained {
		if r.apart[s] && !r.states[s].Now.After(r.now) {
			return true
		}
	}
	return false
}

func keepsTime(s SensorKind) bool { return lasting(s) || s == SensorPanic }

// momentaryUpTo tells whether a momentary sensor is active in a state dated up to t.
func (r *rig) momentaryUpTo(t time.Time) bool {
	for s, sig := range r.states {
		if !lasting(s.sensor) && s.sensor != SensorPanic && sig.Active && !sig.Now.After(t) {
			return true
		}
	}
	return false
}

func (r *rig) sense(s sensorRef, active bool) []Action {
	sig := Signal{Now: r.now, DeviceID: s.dev, Sensor: s.sensor, Active: active}
	if old, ok := r.states[s]; ok {
		r.prev[s] = old
	}
	r.states[s] = sig
	delete(r.blurred, s)
	delete(r.retained, s)
	delete(r.apart, s)
	return r.e.Handle(sig)
}

type opKind int

const (
	opOther opKind = iota
	opDisarm
	opBack
	opRestart
)

// randomOp is one input of a sequence, applied the same way to a rig and its fork.
type randomOp struct {
	name string
	kind opKind
	do   func(r *rig) []Action
}

func pick[T any](rnd *rand.Rand, from ...T) T { return from[rnd.IntN(len(from))] }

func randomOpFor(rnd *rand.Rand, i int) randomOp {
	id := fmt.Sprintf("c%d", i)
	switch x := rnd.IntN(20); {
	case x < 6:
		d := pick(rnd, time.Second, 10*time.Second, 30*time.Second, 45*time.Second, time.Minute, 2*time.Minute, 5*time.Minute, 7*time.Minute)
		return randomOp{name: "advance " + d.String(), do: func(r *rig) []Action { return r.advance(d) }}
	case x < 11:
		s, active := pick(rnd, randomSensors...), rnd.IntN(2) == 0
		return randomOp{name: fmt.Sprintf("%s/%s active=%v", s.dev, s.sensor, active), do: func(r *rig) []Action { return r.sense(s, active) }}
	case x < 13:
		force := rnd.IntN(2) == 0
		return order(fmt.Sprintf("arm force=%v", force), opOther, Command{ID: id, Name: CmdArm, Origin: OriginApp, Force: force})
	case x == 13:
		return order("arm now", opOther, Command{ID: id, Name: CmdArmNow, Origin: OriginApp})
	case x == 14:
		name := pick(rnd, CmdSilence, CmdEnd)
		return order(string(name)+" good code", opOther, codeOrder(id, name, true))
	case x == 15:
		return order("disarm bad code", opOther, codeOrder(id, CmdDisarm, false))
	}
	return clockOp(rnd, id)
}

// clockOp is a restart, a clock set back or forward, a restart with the clock set back
// (a Pi that keeps no time), or a good disarm.
func clockOp(rnd *rand.Rand, id string) randomOp {
	switch rnd.IntN(6) {
	case 0:
		return randomOp{name: "restart", kind: opRestart, do: (*rig).restartAsIs}
	case 5:
		return randomOp{name: "clock caught up, restart", kind: opRestart, do: (*rig).catchUp}
	case 1:
		d := pick(rnd, time.Minute, 10*time.Minute, time.Hour, 3*time.Hour)
		return randomOp{name: "clock back " + d.String(), kind: opBack, do: func(r *rig) []Action {
			r.now = r.now.Add(-d)
			r.blur()
			r.setApart()
			return r.e.Handle(Tick{Now: r.now})
		}}
	case 4:
		d := pick(rnd, time.Minute, 10*time.Minute, 3*time.Hour, 72*time.Hour)
		lost := rnd.IntN(2) == 0
		name := "restart, clock back " + d.String()
		if lost {
			name += ", the broker's last save lost"
		}
		return randomOp{name: name, kind: opRestart, do: func(r *rig) []Action { return r.restartBack(d, lost) }}
	case 2:
		d := pick(rnd, time.Minute, time.Hour, 14*time.Hour)
		return randomOp{name: "clock stepped " + d.String(), do: func(r *rig) []Action {
			r.e.Stepped(d)
			r.now = r.now.Add(d)
			return r.e.Handle(Tick{Now: r.now})
		}}
	}
	return order("disarm good code", opDisarm, codeOrder(id, CmdDisarm, true))
}

// restartAsIs restarts on the clock as it is.
func (r *rig) restartAsIs() []Action {
	r.before = r.e.Mode()
	r.e = restart(r.t, r.harness)
	r.unsure = r.blurredActive() || r.staleBehind()
	if !r.unsure && r.staleCaughtUp() {
		r.caughtUp++
	}
	r.booted = r.boot()
	return r.booted
}

// catchUp lets a clock set back run on past the latest time it reached, ticked but
// never stepped (no internet, or a few minutes back), then restarts.
func (r *rig) catchUp() []Action {
	var out []Action
	if r.reached.After(r.now) {
		r.now = r.reached.Add(time.Minute)
		out = r.e.Handle(Tick{Now: r.now})
	}
	return append(out, r.restartAsIs()...)
}

// restartBack restarts with the clock set back by d, the broker's last save lost or
// not.
func (r *rig) restartBack(d time.Duration, lost bool) []Action {
	r.before = r.e.Mode()
	r.e = restart(r.t, r.harness)
	r.now = r.now.Add(-d)
	if lost {
		r.loseLastSave()
	}
	r.unsure = r.blurredActive() || r.momentaryUpTo(r.now) || r.staleBehind()
	r.blur()
	r.setApart()
	r.booted = r.boot()
	return r.booted
}

func codeOrder(id string, name CommandName, ok bool) Command {
	return Command{ID: id, Name: name, Origin: OriginKeypad, Auth: Auth{Checked: true, OK: ok, User: "u_owner"}}
}

func order(name string, kind opKind, c Command) randomOp {
	return randomOp{name: name, kind: kind, do: func(r *rig) []Action { c.Now = r.now; return r.e.Handle(c) }}
}

// The sequences also reach the class a restart after the clock caught up must handle:
// without it, a time heard before the clock went back and never put back (28 October
// 2026) passed every seed.
func TestRandomSequencesKeepTheAlarmInvariants(t *testing.T) {
	caughtUp := 0
	for seed := range uint64(randomSeeds) {
		caughtUp += runRandom(t, seed)
	}
	if caughtUp < 10 {
		t.Errorf("%d restarts checked after the clock caught up with a time set apart, the broker's state older", caughtUp)
	}
}

// runRandom plays the sequence of one seed, a fork restored from the saved state
// taking the same inputs from a random step on. It returns the restarts checked after
// the clock caught up with a time set apart (see rig.caughtUp).
func runRandom(t *testing.T, seed uint64) int {
	t.Helper()
	rnd := rand.New(rand.NewPCG(seed, 1))
	ops := make([]randomOp, randomSteps)
	for i := range ops {
		ops[i] = randomOpFor(rnd, i)
	}
	at := rnd.IntN(randomSteps)
	fail := func(i int, err error) {
		t.Helper()
		names := make([]string, i+1)
		for j := range names {
			names[j] = ops[j].name
		}
		t.Fatalf("seed %d, step %d (fork at %d): %v\nsequence:\n  %s", seed, i, at, err, strings.Join(names, "\n  "))
	}
	r := newRig(t)
	var twin *rig
	for i, op := range ops {
		if i >= at && twin == nil && !r.blurredActive() && !r.staleBehind() {
			var booted []Action
			twin, booted = r.fork()
			if err := onlySiren(booted); err != nil {
				fail(i, err)
			}
		}
		r.reached = later(r.reached, r.now)
		if twin != nil {
			twin.reached = r.reached
		}
		as, err := r.step(op)
		if err == nil && twin != nil {
			err = sameActions(as, op.do(twin))
		}
		if err != nil {
			fail(i, err)
		}
	}
	if err := r.finish(); err != nil {
		fail(len(ops)-1, err)
	}
	return r.caughtUp
}

// step applies one input and checks what must hold after it.
func (r *rig) step(op randomOp) ([]Action, error) {
	ringing, mode := r.e.State(r.now).Incident, r.e.Mode()
	locked := r.now.Before(r.e.Snapshot().LockedUntil)
	as := op.do(r)
	errs := []error{r.observe(as)}
	switch {
	case op.kind == opDisarm && !locked:
		errs = append(errs, r.disarmed(ringing, as))
	case op.kind == opBack:
		errs = append(errs, pulledBack(mode, r.e.Mode(), as))
	case op.kind == opRestart && !r.unsure:
		errs = append(errs, rebooted(r.before, r.e.Mode(), r.booted))
	}
	errs = append(errs, bounds(r.cfg, r.now, r.e.NextWake(), r.e.Snapshot().LockedUntil))
	return as, errors.Join(errs...)
}

// observe follows the siren orders and the incidents of a batch of actions: the siren
// was last told on exactly while the alarm rings, never for longer than a cycle, and
// no incident goes off or alerts twice.
func (r *rig) observe(as []Action) error {
	for _, a := range as {
		switch v := a.(type) {
		case SirenOn:
			if v.Duration <= 0 || v.Duration > r.cfg.SirenCycle {
				return fmt.Errorf("siren told to sound %v", v.Duration)
			}
			r.sirenOn = true
		case SirenOff:
			r.sirenOn = false
		case Alert:
			if r.alerted[v.Incident] {
				return fmt.Errorf("incident %s alerted twice", v.Incident)
			}
			r.alerted[v.Incident] = true
		case Emit:
			if err := r.event(v.Event); err != nil {
				return err
			}
		}
	}
	st := r.e.State(r.now)
	if rings := st.Mode == ModeTriggered && st.SirenOn; rings != r.sirenOn {
		return fmt.Errorf("the alarm rings: %v, the siren last told on: %v (%+v)", rings, r.sirenOn, st)
	}
	return nil
}

func (r *rig) event(ev Event) error {
	switch v := ev.(type) {
	case Triggered:
		if r.triggered[v.Incident] {
			return fmt.Errorf("incident %s triggered twice", v.Incident)
		}
		r.triggered[v.Incident] = true
	case Cleared:
		r.cleared[v.Incident] = true
	}
	return nil
}

// disarmed checks a disarm with a good code while disarming is not locked: done, the
// alarm that rang cleared and its siren stopped. Only a fire that waited behind it may
// ring then, under its own incident (see wake).
func (r *rig) disarmed(ringing string, as []Action) error {
	var rep Reply
	for _, a := range as {
		if v, ok := a.(Reply); ok {
			rep = v
		}
	}
	if rep.Status != StatusDone {
		return fmt.Errorf("a good code was answered %+v", rep)
	}
	clears := func(c Cleared) bool { return c.Incident == ringing }
	if ringing != "" && (count[SirenOff](as) == 0 || !slices.ContainsFunc(eventsOf[Cleared](as), clears)) {
		return fmt.Errorf("a good code left incident %s ringing: %s", ringing, describe(as))
	}
	st := r.e.State(r.now)
	if st.Mode == ModeDisarmed {
		return nil
	}
	woke := func(tr Triggered) bool { return tr.Incident == st.Incident }
	if st.Mode != ModeTriggered || st.Incident == ringing || !slices.ContainsFunc(eventsOf[Triggered](as), woke) {
		return fmt.Errorf("after a good code: %+v", st)
	}
	return nil
}

// pulledBack checks a clock set back: it does nothing, in the mode it found.
func pulledBack(before, after Mode, as []Action) error {
	if after != before || len(as) > 0 {
		return fmt.Errorf("a clock set back moved %s to %s: %s", before, after, describe(as))
	}
	return nil
}

// rebooted checks a restart: at most the siren told again, in the mode it found. What
// the engine heard before is no news, the clock set back or not.
func rebooted(before, after Mode, as []Action) error {
	if after != before {
		return fmt.Errorf("a restart moved %s to %s: %s", before, after, describe(as))
	}
	return onlySiren(as)
}

// bounds checks that nothing the engine waits for lies further than its own length:
// the longest delay or siren step, the lockout.
func bounds(cfg Config, now, wake, locked time.Time) error {
	if longest := max(cfg.ExitDelay, cfg.EntryDelay, cfg.SirenCycle, cfg.SirenPause); wake.After(now.Add(longest)) {
		return fmt.Errorf("next wake %v after now, longer than any step", wake.Sub(now))
	}
	if locked.After(now.Add(cfg.LockoutDuration)) {
		return fmt.Errorf("locked %v after now, longer than the lockout", locked.Sub(now))
	}
	return nil
}

// finish ends a sequence: the lockout over, a good code disarms, again while a fire
// that waited rings in turn. Every alarm that went off must be cleared by then.
func (r *rig) finish() error {
	r.now = r.now.Add(r.cfg.LockoutDuration)
	if err := r.observe(r.e.Handle(Tick{Now: r.now})); err != nil {
		return err
	}
	for i := 0; r.e.Mode() != ModeDisarmed; i++ {
		if i == 4 {
			return fmt.Errorf("still %s after %d good codes", r.e.Mode(), i)
		}
		c := codeOrder(fmt.Sprintf("end-%d", i), CmdDisarm, true)
		c.Now = r.now
		if err := r.observe(r.e.Handle(c)); err != nil {
			return err
		}
	}
	return allCleared(r.triggered, r.cleared)
}

func allCleared(triggered, cleared map[string]bool) error {
	for _, id := range slices.Sorted(maps.Keys(triggered)) {
		if !cleared[id] {
			return fmt.Errorf("incident %s went off and was never cleared", id)
		}
	}
	return nil
}

// onlySiren checks Boot on the state the live engine has: at most the siren told
// again what it must do.
func onlySiren(as []Action) error {
	for _, a := range as {
		if !isSirenOn(a) {
			return fmt.Errorf("Boot on the live state did %s", describe(as))
		}
	}
	return nil
}

func sameActions(live, restored []Action) error {
	if a, b := describe(live), describe(restored); a != b {
		return fmt.Errorf("the restored engine diverged:\n  live     %s\n  restored %s", a, b)
	}
	return nil
}

// describe writes actions with what their pointers point to, for comparing them.
func describe(as []Action) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		if al, ok := a.(Alert); ok && al.Cause != nil {
			parts = append(parts, fmt.Sprintf("Alert{%s %s %s %+v %v}", al.Incident, al.Kind, al.Urgency, *al.Cause, al.Devices))
			continue
		}
		parts = append(parts, fmt.Sprintf("%T%+v", a, a))
	}
	return strings.Join(parts, " ")
}

// Each check above sees the defect it is there for: fed one, it fails; fed what the
// engine does, it passes.
func TestTheRandomChecksSeeTheirDefects(t *testing.T) {
	r := newRig(t)
	r.armed()
	rang := r.breakWindow()
	if err := r.observe(rang); err != nil {
		t.Fatalf("a real alarm: %v", err)
	}
	if r.observe(rang) == nil {
		t.Error("an incident triggered twice passed")
	}
	if r.sirenOn = false; r.observe(nil) == nil {
		t.Error("an alarm ringing with its siren told off passed")
	}
	ringing := r.e.State(r.now).Incident
	off := r.e.Handle(Command{Now: r.now, ID: "w", Name: CmdDisarm, Auth: Auth{Checked: true, OK: true}})
	if err := r.disarmed(ringing, off); err != nil {
		t.Fatalf("a real disarm: %v", err)
	}
	kept := slices.DeleteFunc(slices.Clone(off), func(a Action) bool { _, ok := a.(SirenOff); return ok })
	if r.disarmed(ringing, kept) == nil || r.disarmed(ringing, nil) == nil {
		t.Error("a good code that leaves the siren on, or is not answered, passed")
	}
	if pulledBack(ModeArmed, ModeArmed, nil) != nil || pulledBack(ModeArmed, ModeDisarmed, nil) == nil {
		t.Error("pulledBack does not tell a clock set back that disarms")
	}
	if rebooted(ModeTriggered, ModeTriggered, []Action{SirenOn{Duration: time.Second}}) != nil ||
		rebooted(ModeArmed, ModeTriggered, nil) == nil || rebooted(ModeDisarmed, ModeDisarmed, []Action{Alert{}}) == nil {
		t.Error("rebooted does not tell a restart that acts on what the engine heard")
	}
	// The sequences reach what rebooted is for: a state the broker kept from before
	// a time heard, the clock gone back behind that time and caught up since.
	caught := newRig(t)
	caught.sense(sensorRef{panic_, SensorPanic}, true)
	caught.advance(30 * time.Second)
	caught.sense(sensorRef{panic_, SensorPanic}, true)
	caught.advance(time.Minute)
	caught.reached = caught.now
	caught.restartBack(3*time.Minute, true)
	if caught.catchUp(); caught.unsure || caught.caughtUp != 1 {
		t.Error("the rig does not see a restart after the clock caught up, the broker's state older")
	}
	cfg := r.cfg
	if bounds(cfg, t0, t0.Add(cfg.SirenCycle), t0.Add(cfg.LockoutDuration)) != nil ||
		bounds(cfg, t0, t0.Add(time.Hour), time.Time{}) == nil || bounds(cfg, t0, time.Time{}, t0.Add(time.Hour)) == nil {
		t.Error("bounds does not tell a deadline or a lockout held too long")
	}
	if allCleared(map[string]bool{"a": true}, map[string]bool{"a": true}) != nil || allCleared(map[string]bool{"a": true}, nil) == nil {
		t.Error("allCleared does not tell an alarm never cleared")
	}
}

// The comparison of a restored engine with the live one sees a state that a restart
// would change: an exit delay saved a minute short arms a minute early.
func TestTheRestoredComparisonSeesADivergence(t *testing.T) {
	live := newRig(t)
	live.arm()
	same, booted := live.fork()
	if err := onlySiren(booted); err != nil {
		t.Fatal(err)
	}
	short, _ := live.fork()
	snap := short.e.Snapshot()
	snap.Deadline = snap.Deadline.Add(-time.Minute)
	short.e.Restore(snap)
	short.boot()
	got := live.advance(time.Minute)
	if err := sameActions(got, same.advance(time.Minute)); err != nil {
		t.Fatalf("an untouched fork: %v", err)
	}
	if sameActions(got, short.advance(time.Minute)) == nil {
		t.Fatal("an exit delay saved a minute short passed")
	}
}
