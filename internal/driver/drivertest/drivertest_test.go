package drivertest_test

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/driver"
	"safehome/internal/driver/drivertest"
)

var (
	_ driver.Clock = (*drivertest.FakeClock)(nil)
	_ driver.Store = (*drivertest.MemStore)(nil)
)

var t0 = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

func fired(c <-chan time.Time) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

func TestTheClockFiresTimersWhenTheyAreDue(t *testing.T) {
	c := drivertest.NewFakeClock(t0)
	soon, _ := c.Timer(time.Second)
	late, _ := c.Timer(time.Minute)
	stopped, stop := c.Timer(time.Second)
	stop()
	if now, _ := c.Timer(0); !fired(now) {
		t.Fatal("a timer of zero fires at once")
	}
	if !c.HasTimer(t0.Add(time.Minute)) || c.HasTimer(t0.Add(2*time.Minute)) {
		t.Fatal("HasTimer")
	}
	c.Advance(time.Second)
	if !fired(soon) || fired(late) || fired(stopped) || !c.Now().Equal(t0.Add(time.Second)) {
		t.Fatal("after a second, only the timer of a second fires")
	}
	c.Set(t0.Add(time.Hour))
	if fired(late) {
		t.Fatal("setting the time fires no timer")
	}
	c.Set(t0)
	if !c.Now().Equal(t0) {
		t.Fatal("Set goes backwards too")
	}
}

func TestTimersFollowTheMonotonicClockNotTheTimeRead(t *testing.T) {
	c := drivertest.NewFakeClock(t0)
	minute, _ := c.Timer(time.Minute)
	c.Set(t0.Add(-time.Hour)) // corrected backwards: the timer still fires a minute on
	if !c.HasTimer(t0.Add(-time.Hour).Add(time.Minute)) {
		t.Fatal("HasTimer reads the timer on the clock as it is now")
	}
	c.Advance(59 * time.Second)
	if fired(minute) {
		t.Fatal("fired early")
	}
	c.Advance(time.Second)
	if !fired(minute) {
		t.Fatal("a timer fires once its duration passed, whatever the time read says")
	}
	hour, _ := c.Timer(time.Hour)
	c.Set(t0.Add(48 * time.Hour)) // corrected forwards: nothing fires before its time
	c.Advance(time.Minute)
	if fired(hour) {
		t.Fatal("setting the time forward fired a timer")
	}
}

func TestPassTimerWaitsForTheTimer(t *testing.T) {
	c := drivertest.NewFakeClock(t0)
	got := make(chan time.Time)
	go func() {
		time.Sleep(5 * time.Millisecond) // the code under test takes a moment to wait
		fired, _ := c.Timer(time.Minute)
		got <- <-fired
	}()
	c.PassTimer(t, time.Minute)
	if at := <-got; !at.Equal(t0.Add(time.Minute)) {
		t.Fatalf("fired at %v", at)
	}
}

func TestTheStoreKeepsCountsAndFailsWhenTold(t *testing.T) {
	s := drivertest.NewMemStore()
	if _, found, err := s.Load("a"); found || err != nil {
		t.Fatal("nothing saved yet")
	}
	data := []byte("12")
	s.Save("a", data)
	data[0] = 'x' // the store keeps its own copy
	s.FailSaves(1)
	if err := s.Save("a", []byte("345")); !errors.Is(err, drivertest.ErrDiskFull) {
		t.Fatalf("a save told to fail: %v", err)
	}
	s.Save("a", []byte("678"))
	if got, found, _ := s.Load("a"); !found || string(got) != "678" {
		t.Fatalf("%q", got)
	}
	if s.Saves("a") != 2 || fmt.Sprint(s.Sizes("a")) != "[2 3]" || s.Saves("b") != 0 {
		t.Fatalf("%d saves of sizes %v", s.Saves("a"), s.Sizes("a"))
	}
	broken := errors.New("corrupt")
	s.FailLoads(broken)
	if _, _, err := s.Load("a"); err != broken {
		t.Fatal(err)
	}
	s.FailLoads(nil)
	if _, found, _ := s.Load("a"); !found {
		t.Fatal("loads work again")
	}
}

func TestTheBufferTakesConcurrentLogs(t *testing.T) {
	logs := drivertest.Logs(t)
	log := slog.New(slog.NewTextHandler(logs, nil))
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { log.Info("hello") })
	}
	wg.Wait()
	if n := strings.Count(logs.String(), "msg=hello"); n != 10 {
		t.Fatalf("%d lines", n)
	}
}

func TestWaitForReturnsOnceTheConditionHolds(t *testing.T) {
	start := time.Now()
	n := 0
	drivertest.WaitFor(t, "three polls", func() bool { n++; return n == 3 })
	if n != 3 || time.Since(start) > time.Second {
		t.Fatalf("%d polls in %v", n, time.Since(start))
	}
}
