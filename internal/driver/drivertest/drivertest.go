// Package drivertest fakes the driver's ports for tests: a clock that moves only
// when told, a store in memory, a log buffer goroutines can share, and WaitFor for
// what happens on another goroutine. Only tests may import it (see
// internal/archtest). It knows nothing of the module, so that the driver's own
// tests can use it.
package drivertest

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

// FakeClock is a driver.Clock that moves only when told.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*timer]struct{}
}

type timer struct {
	when  time.Time
	fired chan time.Time
}

// NewFakeClock returns a clock that reads now until it is moved.
func NewFakeClock(now time.Time) *FakeClock {
	return &FakeClock{now: now, timers: map[*timer]struct{}{}}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Timer fires once the clock is moved d forward. A timer of zero or less fires at
// once, as with the real clock.
func (c *FakeClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &timer{when: c.now.Add(d), fired: make(chan time.Time, 1)}
	if d <= 0 {
		t.fired <- c.now
		return t.fired, func() {}
	}
	c.timers[t] = struct{}{}
	return t.fired, func() { c.mu.Lock(); delete(c.timers, t); c.mu.Unlock() }
}

// Advance moves time forward by d and fires the timers that are due.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for t := range c.timers {
		if !t.when.After(c.now) {
			t.fired <- c.now
			delete(c.timers, t)
		}
	}
}

// Set changes the time read, backwards too, and fires no timer: a system clock that
// is corrected, while timers follow the monotonic clock.
func (c *FakeClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// HasTimer reports whether a timer waits to fire at when.
func (c *FakeClock) HasTimer(when time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for t := range c.timers {
		if t.when.Equal(when) {
			return true
		}
	}
	return false
}

// WaitTimer waits until a timer waits to fire at when: the code under test has
// reached the point where it waits for time to pass.
func (c *FakeClock) WaitTimer(t testing.TB, when time.Time) {
	t.Helper()
	WaitFor(t, "a timer at "+when.Format(time.RFC3339Nano), func() bool { return c.HasTimer(when) })
}

// PassTimer waits until a timer of d from now is set, then lets d pass.
func (c *FakeClock) PassTimer(t testing.TB, d time.Duration) {
	t.Helper()
	c.WaitTimer(t, c.Now().Add(d))
	c.Advance(d)
}

// MemStore is a driver.Store in memory. It can be told to fail.
type MemStore struct {
	mu       sync.Mutex
	docs     map[string][]byte
	sizes    map[string][]int
	failSave int // the next n saves fail
	loadErr  error
}

// ErrDiskFull is what a save made to fail answers.
var ErrDiskFull = errors.New("drivertest: disk full")

func NewMemStore() *MemStore {
	return &MemStore{docs: map[string][]byte{}, sizes: map[string][]int{}}
}

func (s *MemStore) Save(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSave > 0 {
		s.failSave--
		return ErrDiskFull
	}
	s.docs[name] = bytes.Clone(data)
	s.sizes[name] = append(s.sizes[name], len(data))
	return nil
}

func (s *MemStore) Load(name string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, false, s.loadErr
	}
	d, ok := s.docs[name]
	return bytes.Clone(d), ok, nil
}

// FailSaves has the next n saves fail with ErrDiskFull.
func (s *MemStore) FailSaves(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failSave = n
}

// FailLoads has every load fail with err; nil makes them work again.
func (s *MemStore) FailLoads(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadErr = err
}

// Saves returns how many times name was saved.
func (s *MemStore) Saves(name string) int { return len(s.Sizes(name)) }

// Sizes returns the size of each save of name, in order.
func (s *MemStore) Sizes(name string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.sizes[name]...)
}

// SyncBuffer is an io.Writer goroutines can share, for a test's logs.
type SyncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *SyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *SyncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Logs returns a buffer for the logs of the code under test, printed if the test
// fails.
func Logs(t testing.TB) *SyncBuffer {
	b := &SyncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("logs:\n%s", b)
		}
	})
	return b
}

// waitLimit is long: it is reached only by a failing test, and a loaded machine
// running -race, with real processes, may be slow.
const waitLimit = 20 * time.Second

// WaitFor waits until cond holds, or fails the test. It polls often at first, so
// that a quick condition is seen at once, then less, so that a slow one (a process,
// a broker) is not pressed.
func WaitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for pause := time.Millisecond; !cond(); pause = min(2*pause, 10*time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", waitLimit, what)
		}
		time.Sleep(pause)
	}
}
