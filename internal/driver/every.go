package driver

import (
	"context"
	"time"
)

// Every runs fn every d on clock until ctx ends: what an application repeats, such as
// a status the hub watches or a heartbeat. Each wait starts when fn returns, so a
// repetition that goes through a loop that hangs stops, and whoever watches it
// notices.
func Every(ctx context.Context, clock Clock, d time.Duration, fn func(context.Context)) {
	for {
		fired, stop := clock.Timer(d)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-fired:
			fn(ctx)
		}
	}
}
