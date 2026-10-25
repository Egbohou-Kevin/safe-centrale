package driver

import (
	"context"
	"strings"
	"testing"
	"time"

	"safehome/internal/driver/drivertest"
	"safehome/internal/engine"
)

// The real clock's readings carry a monotonic reading, and Go compares two times that
// both carry one by that reading alone: a system clock stepped back would go unseen,
// and the guard that keeps the domain's time from going back would do nothing (the
// fake clock carries none, which hid it). The loop hands the core wall time only.
func TestTheCoreSeesWallTimeOnly(t *testing.T) {
	e, err := engine.New(testEngineConfig(), func() string { return "inc" }, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	loop, err := New(Options[engine.Input, engine.Action, engine.Snapshot]{
		Name: "engine", Core: engineCore(e), Clock: SystemClock(), Store: drivertest.NewMemStore(),
		Dispatch: func(context.Context, time.Time, []engine.Action) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	var seen []time.Time
	for range 2 {
		loop.Do(ctx, func(now time.Time) []engine.Action { seen = append(seen, now); return nil })
	}
	for _, now := range seen {
		if strings.Contains(now.String(), " m=") || !now.Equal(now.Round(0)) {
			t.Fatalf("a time with a monotonic reading reached the core: %v", now)
		}
	}
}
