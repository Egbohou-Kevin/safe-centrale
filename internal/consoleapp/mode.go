package consoleapp

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"safehome/internal/contract"
	"safehome/internal/install"
)

// mode is one installation mode, as Run follows it.
type mode struct {
	cancel    context.CancelFunc // ends the page; the network then goes down
	deadline  <-chan time.Time   // the mode's end, should the hub say nothing
	stopTimer func()
	down      chan struct{} // closed once the network is down again
	failed    atomic.Bool   // the network did not start
}

// Run serves the console until ctx is cancelled. Its loop never waits for the
// network, which nmcli may take long to start or stop: it keeps answering Alive.
//
// It starts by taking down a network a console that died during a mode may have
// left (a crash, the watchdog): nothing else would, should that mode be over by now,
// and the hub's Wi-Fi would stay on for good (and off the box's network, the same
// chip cannot hold both).
func (a *App) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Go(func() { a.worker(ctx) })
	defer wg.Wait()

	// The open mode (nil when closed), and the latest one, whose network the next
	// one waits to be down: first, the cleanup of the start.
	var current *mode
	last := a.cleanup(ctx)
	defer func() {
		a.end(current, "the console stops")
		if last != nil {
			<-last.down // the network is stopped on the way out
		}
	}()
	for {
		var deadline <-chan time.Time
		if current != nil {
			deadline = current.deadline
		}
		select {
		case <-ctx.Done():
			return nil
		case reply := <-a.ping:
			close(reply)
		case <-deadline:
			a.end(current, contract.InstallTimeout) // the hub says so too, unless it is gone
			current = nil
		case d := <-a.modes:
			switch {
			case d.Status == contract.InstallClosed:
				a.end(current, d.Reason)
				current = nil
			case current == nil || current.failed.Load() && d.Until.After(a.ends()):
				// A new mode; or the installer typed the code again, as the network
				// did not come: it is started again, for the new half hour.
				a.end(current, "the network did not start: once more")
				current = nil
				if m := a.begin(ctx, d.Until.Time, last); m != nil {
					current, last = m, m
				}
			default:
				a.extend(current, d.Until.Time)
			}
		}
	}
}

// begin opens an installation mode lasting until until, unless that time is past
// (an old retained message). The network comes up in the background, once the
// previous mode's is down.
func (a *App) begin(ctx context.Context, until time.Time, prev *mode) *mode {
	now := a.deps.Clock.Now()
	left := until.Sub(now)
	if left <= 0 {
		return nil
	}
	sctx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.open, a.opened, a.until, a.session = true, now, until, sctx
	a.events, a.pairing, a.hotspotErr = nil, nil, ""
	a.mu.Unlock()
	a.log.Warn("installation mode open: the network and the page start", "until", until.Format(time.RFC3339))

	m := &mode{cancel: cancel, down: make(chan struct{})}
	m.deadline, m.stopTimer = a.deps.Clock.Timer(left)
	go func() {
		defer close(m.down)
		if prev != nil {
			<-prev.down
		}
		a.network(ctx, sctx, m)
	}()
	return m
}

// cleanup takes down, in the background, a network an earlier run left; the mode
// it returns is down once that is done.
func (a *App) cleanup(ctx context.Context) *mode {
	m := &mode{down: make(chan struct{})}
	go func() {
		defer close(m.down)
		downCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.Wait)
		defer cancel()
		if err := a.deps.Hotspot.Down(downCtx); err != nil {
			a.log.Debug("no network left by an earlier run to take down", "err", err)
		}
	}()
	return m
}

// ends is when the open mode ends.
func (a *App) ends() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.until
}

// extend moves the end of the open mode m to until, if that is later: the installer
// typed the code again, and the hub started a new half hour.
func (a *App) extend(m *mode, until time.Time) {
	a.mu.Lock()
	later := until.After(a.until)
	if later {
		a.until = until
	}
	a.mu.Unlock()
	if !later {
		return // the same message again
	}
	a.log.Info("installation mode extended", "until", until.Format(time.RFC3339))
	m.stopTimer()
	m.deadline, m.stopTimer = a.deps.Clock.Timer(until.Sub(a.deps.Clock.Now()))
}

// end closes the installation mode m, if any: the orders are refused at once, the
// page and the network stop in the background.
func (a *App) end(m *mode, why string) {
	if m == nil {
		return
	}
	a.log.Info("installation mode over: the network and the page stop", "reason", why)
	m.stopTimer()
	m.cancel()
	a.mu.Lock()
	a.open, a.session = false, nil
	a.mu.Unlock()
}

// network brings the network of the mode m up, serves the page until the mode's
// context sctx ends, then brings the network down, even when ctx is over too. A
// network that does not start is started again if the installer types the code
// again (see Run).
func (a *App) network(ctx, sctx context.Context, m *mode) {
	upCtx, cancel := context.WithTimeout(sctx, a.cfg.Wait)
	err := a.deps.Hotspot.Up(upCtx)
	cancel()
	switch {
	case err != nil && sctx.Err() == nil:
		a.log.Error("the console's network did not start: the page cannot be reached; type the installer code again to try once more", "err", err)
		m.failed.Store(true)
		a.mu.Lock()
		if a.session == sctx {
			a.hotspotErr = err.Error()
		}
		a.mu.Unlock()
		<-sctx.Done() // a start that failed may still bring the network up: down at the end
	case err == nil:
		if err := a.deps.Serve(sctx); err != nil && sctx.Err() == nil {
			a.log.Error("the console's page stopped", "err", err)
		}
		<-sctx.Done()
	}
	// Also after a start cut short or failed: it may have left a network.
	downCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.Wait)
	defer cancel()
	if err := a.deps.Hotspot.Down(downCtx); err != nil {
		a.log.Error("the console's network may still be on", "err", err)
	}
}

// worker owns the installer's session: it follows the broker, and runs the orders of
// the page (a pairing, a removal) one at a time.
func (a *App) worker(ctx context.Context) {
	for {
		var next func(context.Context)
		fctx, cancel := context.WithCancel(ctx)
		got := make(chan struct{})
		go func() {
			defer close(got)
			select {
			case next = <-a.jobs:
				cancel()
			case <-fctx.Done():
			}
		}()
		a.sess.Follow(fctx)
		cancel()
		<-got
		if ctx.Err() != nil {
			return
		}
		if next != nil {
			next(ctx)
			if v, ok := a.sess.Current(); ok {
				a.setView(v)
			}
		}
	}
}

func (a *App) setView(v install.View) {
	a.mu.Lock()
	a.view, a.known = v, true
	a.mu.Unlock()
}

func (a *App) addEvent(at time.Time, e contract.DeviceEventData) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.open || at.Before(a.opened) {
		return // from before the mode: the worker may read it late
	}
	a.events = append(a.events, Event{At: at, DeviceID: e.DeviceID, Capability: e.Capability, Value: e.StringValue()})
	if len(a.events) > a.cfg.MaxEvents {
		a.events = a.events[len(a.events)-a.cfg.MaxEvents:]
	}
}
