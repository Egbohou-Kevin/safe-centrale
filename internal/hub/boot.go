package hub

import (
	"context"
	"maps"
	"slices"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
	"safehome/internal/health"
)

// boot lets the retained device states arrive, then tells the engine what the
// devices look like, so a door left open is known before the first event.
func (h *Hub) boot(ctx context.Context) error {
	if err := h.bootWindow(ctx); err != nil {
		return err
	}
	signals := h.endBootWindow()
	err := h.alarm.loop.Do(ctx, func(now time.Time) []engine.Action {
		actions := h.alarm.eng.Boot(now, h.booted(now), signals)
		h.publishState(now) // the first retained state, even if nothing changed since the last run
		// An installation mode never survives a restart: one left open by a crash is
		// closed for the console too. One opened at the keypad during the boot window
		// is this run's own: it stands, and the console keeps its network.
		if !h.alarm.inst.open {
			h.publishInstall(now, contract.InstallRestart)
		}
		return actions
	})
	if err != nil {
		return err
	}
	return h.monitor.loop.Do(ctx, func(now time.Time) []health.Action {
		h.publishHealth(now)
		h.monitor.booted = true
		h.publishDevices(now)
		h.publishContacts(now)
		return nil
	})
}

// bootedAt is when the system started, on the clock as it is now: now less the
// system's uptime. The zero time when the uptime is unknown, or when it would put the
// start after the hub's own (a wrong reading): the engine then keeps the rule without
// it, and takes as news every state dated up to now.
func (h *Hub) bootedAt(now time.Time) time.Time {
	if h.deps.Uptime == nil {
		return time.Time{}
	}
	up, ok := h.deps.Uptime()
	if !ok || up <= 0 {
		return time.Time{}
	}
	booted := now.Add(-up)
	if booted.After(h.started) {
		h.log.Warn("the system's uptime puts its start after the hub's, ignored", "uptime", up)
		return time.Time{}
	}
	return booted
}

// booted is when the system started, for the engine: on the clock as it is now
// (bootedAt), and on the clock as it read early at boot when NTP set it forward since,
// before the hub started. The time read at boot unknown, or not before the start on
// the clock as it is now (no step forward, or a wrong reading: after it, after now),
// the engine keeps the rule of the start alone.
func (h *Hub) booted(now time.Time) engine.Booted {
	b := engine.Booted{At: h.bootedAt(now)}
	if b.At.IsZero() || h.deps.StartShown == nil {
		return b
	}
	shown, ok := h.deps.StartShown()
	if !ok || !shown.Before(b.At) {
		return b
	}
	h.log.Info("the clock was set forward since the system started", "by", b.At.Sub(shown))
	b.Shown = shown
	return b
}

// bootWindow waits for the link to come up (LinkWait at most), then for the retained
// device states to arrive (BootWindow).
func (h *Hub) bootWindow(ctx context.Context) error {
	if h.cfg.BootWindow == 0 {
		return nil
	}
	if h.cfg.LinkWait > 0 {
		gaveUp, stop := h.deps.Clock.Timer(h.cfg.LinkWait)
		defer stop()
		select {
		case <-h.startup.linkUp:
		case <-gaveUp:
			h.log.Warn("the broker link did not come up in time, listening for device states anyway")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	over, stop := h.deps.Clock.Timer(h.cfg.BootWindow)
	defer stop()
	select {
	case <-over:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// endBootWindow stops collecting retained states and returns what they say to the
// engine, in a stable order, each dated when it began (see retainedSignal).
func (h *Hub) endBootWindow() []engine.Signal {
	h.startup.mu.Lock()
	h.startup.open = false
	states := h.startup.states
	h.startup.states = nil
	h.startup.mu.Unlock()

	var signals []engine.Signal
	for _, key := range slices.Sorted(maps.Keys(states)) {
		if sig, ok := retainedSignal(states[key]); ok {
			signals = append(signals, sig)
		}
	}
	return signals
}
