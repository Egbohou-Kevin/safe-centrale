// Package driver runs a domain core in real time. A domain core (the alarm engine,
// the health monitor) is deterministic and knows nothing of the clock, the disk or
// the network; the loop here is what feeds it.
//
// One goroutine owns the core. Everything that wants to reach it (a sensor
// message, an order from the app, a deadline) becomes a task that runs on that
// goroutine, one at a time, so the core never needs a lock. After each task the
// loop sends the resulting actions to the outside world, then saves the core's
// state.
//
// The loop is generic: it does not import the engine or the monitor, only
// describes what it needs from them (Core). Time, storage and output are ports,
// so tests run it with a simulated clock.
package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ErrStopped is returned when the loop is not running any more.
var ErrStopped = errors.New("driver: loop stopped")

// Clock is the source of time. The domain cores never see it: the loop reads it
// and stamps each input.
type Clock interface {
	Now() time.Time
	// Timer fires once, d from now. stop cancels it and frees its resources.
	Timer(d time.Duration) (fired <-chan time.Time, stop func())
}

// Store keeps the state between runs. See internal/adapter/filestore.
type Store interface {
	Save(name string, data []byte) error
	// Load returns found=false if there is no saved state yet.
	Load(name string) (data []byte, found bool, err error)
}

// Core is what the loop needs from a domain core, as functions so that any core
// fits (the engine and the monitor have different input and action types).
type Core[In, Act, Snap any] struct {
	Handle   func(In) []Act     // process one input
	NextWake func() time.Time   // when the core wants a tick, zero if never
	Snapshot func() Snap        // the state to save
	Restore  func(Snap)         // put a saved state back
	Tick     func(time.Time) In // build the input that means "time has passed"
}

// Options configures a loop.
type Options[In, Act, Snap any] struct {
	// Name is the key under which the state is saved ("engine", "health").
	Name  string
	Core  Core[In, Act, Snap]
	Clock Clock
	Store Store
	// Dispatch carries out the actions: publish messages, drive the siren. It runs
	// on the loop's goroutine, so it must not block for long. A returned error is
	// logged; the loop goes on, because an alarm that stops reacting is worse than
	// one that failed to send a message.
	Dispatch func(ctx context.Context, now time.Time, actions []Act) error
	// OnChange, if set, runs after a task that changed the core's state, whatever the
	// outcome of the save, on the loop's goroutine and under the same rule as
	// Dispatch: it must not block for long. It is where the retained state is
	// published, for instance. It never runs at construction.
	OnChange func(ctx context.Context, now time.Time)
	Logger   *slog.Logger
}

// task is one unit of work for the loop's goroutine.
type task[Act any] struct {
	run  func(now time.Time) []Act // nil: nothing to run (Alive)
	done chan struct{}             // closed once the task is fully processed; nil if nobody waits
}

// Loop runs one core.
type Loop[In, Act, Snap any] struct {
	o       Options[In, Act, Snap]
	log     *slog.Logger
	tasks   chan task[Act]
	stopped chan struct{}
	last    time.Time // latest time handed to the core: time never goes back
	saved   []byte    // last state written, to skip identical writes
	seen    []byte    // last state observed, to tell OnChange something moved
}

const queueSize = 256

// New builds a loop and puts the saved state back into the core, if there is one.
// A core that was restored usually needs a Boot step (see Engine.Boot) which the
// application runs with Do.
//
// If the saved state is damaged, New fails: the application must decide whether
// to start empty, because for an alarm that is a decision, not a default.
func New[In, Act, Snap any](o Options[In, Act, Snap]) (*Loop[In, Act, Snap], error) {
	switch {
	case o.Name == "":
		return nil, errors.New("driver: Name is required")
	case o.Clock == nil || o.Store == nil || o.Dispatch == nil:
		return nil, errors.New("driver: Clock, Store and Dispatch are required")
	case o.Core.Handle == nil || o.Core.NextWake == nil || o.Core.Snapshot == nil || o.Core.Restore == nil || o.Core.Tick == nil:
		return nil, errors.New("driver: every Core function is required")
	}
	l := &Loop[In, Act, Snap]{
		o:       o,
		log:     o.Logger,
		tasks:   make(chan task[Act], queueSize),
		stopped: make(chan struct{}),
	}
	if l.log == nil {
		l.log = slog.New(slog.DiscardHandler)
	}
	l.log = l.log.With("core", o.Name)

	data, found, err := o.Store.Load(o.Name)
	if err != nil {
		return nil, fmt.Errorf("driver: loading %s: %w", o.Name, err)
	}
	if found {
		var snap Snap
		if err := json.Unmarshal(data, &snap); err != nil {
			return nil, fmt.Errorf("driver: reading saved %s: %w", o.Name, err)
		}
		o.Core.Restore(snap)
		l.saved = data
	}
	// What the core looks like now is the reference for "something changed".
	if b, err := json.Marshal(o.Core.Snapshot()); err == nil {
		l.seen = b
	}
	return l, nil
}

// now reads the clock, never going back: if the system clock steps backward (a
// clock correction, a dying RTC battery), the core keeps seeing time that moves
// forward.
func (l *Loop[In, Act, Snap]) now() time.Time {
	t := l.o.Clock.Now()
	if t.Before(l.last) {
		t = l.last
	}
	l.last = t
	return t
}

// Run serves the core until ctx is cancelled. It must be called once.
func (l *Loop[In, Act, Snap]) Run(ctx context.Context) error {
	defer close(l.stopped)
	for {
		var fired <-chan time.Time
		stop := func() {}
		if wake := l.o.Core.NextWake(); !wake.IsZero() {
			fired, stop = l.o.Clock.Timer(max(wake.Sub(l.o.Clock.Now()), 0))
		}
		select {
		case <-ctx.Done():
			stop()
			return ctx.Err()
		case t := <-l.tasks:
			stop()
			if t.run != nil {
				l.process(ctx, t.run)
			}
			if t.done != nil {
				close(t.done)
			}
		case <-fired:
			l.process(ctx, func(now time.Time) []Act { return l.o.Core.Handle(l.o.Core.Tick(now)) })
		}
	}
}

func (l *Loop[In, Act, Snap]) process(ctx context.Context, run func(time.Time) []Act) {
	now := l.now()
	actions := run(now)
	// Effects first, state second: if the hub dies in between, the siren and the
	// alerts have already gone out, which is the lesser evil compared to a state
	// saved for actions nobody carried out.
	if len(actions) > 0 {
		if err := l.o.Dispatch(ctx, now, actions); err != nil {
			l.log.Error("dispatching actions failed", "err", err, "actions", len(actions))
		}
	}
	if l.persist() && l.o.OnChange != nil {
		l.o.OnChange(ctx, now)
	}
}

// persist saves the state if it changed, and reports whether it moved since the
// last task. The two are separate: a save that failed is retried on the next task
// without announcing the same change twice. Skipping identical writes spares the SD
// card: most inputs (a battery reading, a door closing) leave the state unchanged
// or change it only a few times a day.
func (l *Loop[In, Act, Snap]) persist() (changed bool) {
	data, err := json.Marshal(l.o.Core.Snapshot())
	if err != nil {
		l.log.Error("encoding the state failed", "err", err)
		return false
	}
	changed = !bytes.Equal(data, l.seen)
	l.seen = data
	if bytes.Equal(data, l.saved) {
		return changed
	}
	if err := l.o.Store.Save(l.o.Name, data); err != nil {
		l.log.Error("saving the state failed, will retry on the next change", "err", err)
		return changed
	}
	l.saved = data
	return changed
}

// Submit queues an input. build runs on the loop's goroutine with the time of the
// moment, and returns the input to hand to the core. It returns when the input is
// queued, not when it has been processed, so a message handler is never held up.
func (l *Loop[In, Act, Snap]) Submit(ctx context.Context, build func(now time.Time) In) error {
	return l.enqueue(ctx, task[Act]{run: func(now time.Time) []Act { return l.o.Core.Handle(build(now)) }})
}

// Do runs fn on the loop's goroutine and waits until it is fully processed: its
// actions dispatched and the state saved. It is how the application runs what is
// not an input, such as Engine.Boot, and it is also the only safe way to read the
// core from another goroutine (the core belongs to the loop). When Do returns,
// everything queued before it has been processed too.
func (l *Loop[In, Act, Snap]) Do(ctx context.Context, fn func(now time.Time) []Act) error {
	return l.runAndWait(ctx, fn)
}

// Alive returns once the loop has taken everything queued before it: the loop's
// goroutine is not stuck (in the core, in Dispatch or on the disk). It does not
// touch the core. It is what a program checks before telling systemd that it still
// works (the watchdog).
func (l *Loop[In, Act, Snap]) Alive(ctx context.Context) error {
	return l.runAndWait(ctx, nil)
}

// runAndWait queues fn (nil: nothing to run) and waits until the loop is done with it.
func (l *Loop[In, Act, Snap]) runAndWait(ctx context.Context, fn func(now time.Time) []Act) error {
	done := make(chan struct{})
	if err := l.enqueue(ctx, task[Act]{run: fn, done: done}); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-l.stopped:
		return ErrStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Post queues fn like Do, without waiting for it: for a message handler that must
// return quickly but needs more than an input (an input and a publication, say).
func (l *Loop[In, Act, Snap]) Post(ctx context.Context, fn func(now time.Time) []Act) error {
	return l.enqueue(ctx, task[Act]{run: fn})
}

func (l *Loop[In, Act, Snap]) enqueue(ctx context.Context, t task[Act]) error {
	select {
	case <-l.stopped:
		return ErrStopped
	default:
	}
	select {
	case l.tasks <- t:
		return nil
	case <-l.stopped:
		return ErrStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SystemClock is the real clock.
func SystemClock() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}
