package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// bigItem looks like an item of the link agent's queue, which may hold 10 MB.
type bigItem struct {
	ID      string          `json:"id"`
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload"`
	At      time.Time       `json:"at"`
}

type bigSnap struct{ Queue []bigItem }

// BenchmarkInputThatChangesNothing measures what an input costs the loop when the
// core's state is large (20000 items of 500 bytes) and the input leaves it as is.
func BenchmarkInputThatChangesNothing(b *testing.B) {
	payload := json.RawMessage(`{"data":"` + strings.Repeat("x", 480) + `"}`)
	snap := bigSnap{Queue: make([]bigItem, 20000)}
	for i := range snap.Queue {
		snap.Queue[i] = bigItem{ID: fmt.Sprintf("id%05d", i), Topic: "safehome/s/device/d/event", Payload: payload, At: t0}
	}
	l, err := New(Options[int, int, bigSnap]{
		Name: "big", Clock: newFakeClock(), Store: newMemStore(),
		Core: Core[int, int, bigSnap]{
			Handle:   func(int) []int { return nil },
			NextWake: func() time.Time { return time.Time{} },
			Snapshot: func() bigSnap { return snap },
			Restore:  func(bigSnap) {},
			Tick:     func(time.Time) int { return 0 },
		},
		Dispatch: func(context.Context, time.Time, []int) error { return nil },
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		l.process(context.Background(), func(time.Time) []int { return nil })
	}
}
