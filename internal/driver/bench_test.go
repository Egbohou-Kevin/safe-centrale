package driver

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// bigSnap looks like the link agent's state, the largest a core saves: the ids of
// the last 2048 messages from the cloud (some 60 KB), and the incidents it follows.
type bigSnap struct {
	Seen      []string             `json:"seen"`
	Incidents map[string]time.Time `json:"incidents"`
}

// BenchmarkInputThatChangesNothing measures what an input costs the loop when the
// core's state is large and the input leaves it as is.
func BenchmarkInputThatChangesNothing(b *testing.B) {
	snap := bigSnap{Seen: make([]string, 2048), Incidents: map[string]time.Time{}}
	for i := range snap.Seen {
		snap.Seen[i] = fmt.Sprintf("01JB%022d", i) // a ULID's length
	}
	for i := range 20 {
		snap.Incidents[fmt.Sprintf("inc%02d", i)] = t0
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
