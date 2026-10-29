package hub

import (
	"context"
	"time"

	"safehome/internal/driver"
	"safehome/internal/health"
)

// announcement is what Deps.Updating read last.
type announcement struct {
	until, made time.Time
	err         string
}

// followUpdating reads the update announcement (Deps.Updating) at start, then every
// UpdatingPoll, and hands each change to the monitor: system.health then says it, or
// no longer. One that cannot be read counts as none, so that a damaged file never hides
// a fault. Nothing of the alarm follows it. Nil Deps.Updating: nothing runs.
func (h *Hub) followUpdating(ctx context.Context) {
	if h.deps.Updating == nil {
		return
	}
	var last *announcement
	read := func(context.Context) {
		until, made, err := h.deps.Updating()
		got := announcement{until: until, made: made}
		if err != nil {
			got = announcement{err: err.Error()}
		}
		if last != nil && got.until.Equal(last.until) && got.made.Equal(last.made) && got.err == last.err {
			return
		}
		last = &got
		if err != nil {
			h.log.Error("the update announcement cannot be read: none is announced", "err", err)
		}
		h.submitFailed(h.monitor.loop.Submit(ctx, func(t time.Time) health.Input {
			return health.Updating{Now: t, Until: got.until, Made: got.made}
		}))
	}
	read(ctx)
	driver.Every(ctx, h.deps.Clock, h.cfg.UpdatingPoll, read)
}
