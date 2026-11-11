package driver

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"safehome/internal/engine"
)

// A task the loop took is carried out even when the loop stops: a message handler
// tells the broker it took a message once Submit returned nil, and the broker forgets
// it. After the stop, nothing is taken.
func TestEveryTaskTakenIsCarriedOutWhenTheLoopStops(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	hold, held := make(chan struct{}), make(chan struct{})
	r.loop.Post(r.ctx, func(time.Time) []engine.Action { close(held); <-hold; return nil })
	<-held
	var ran atomic.Int32
	build := func(now time.Time) engine.Input { ran.Add(1); return engine.Tick{Now: now} }
	for range 50 {
		if err := r.loop.Submit(r.ctx, build); err != nil {
			t.Fatal(err)
		}
	}
	r.cancel()
	close(hold)
	if err := r.stop(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if n := ran.Load(); n != 50 {
		t.Fatalf("%d tasks of 50 carried out", n)
	}
	if err := r.loop.Submit(context.Background(), build); !errors.Is(err, ErrStopped) {
		t.Fatalf("taken after the stop: %v", err)
	}
}
