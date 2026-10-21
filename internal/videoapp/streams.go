package videoapp

import (
	"context"
	"errors"
	"time"

	"safehome/internal/video"
)

// stopping reports whether Run is over: nothing new starts.
func (a *App) stopping() bool { return a.life.Err() != nil }

// away runs f in its own goroutine, which Run waits for at the end. Called on the
// loop's goroutine, so that it never waits for a camera.
func (a *App) away(f func()) {
	a.work.Add(1)
	go func() {
		defer a.work.Done()
		f()
	}()
}

// startStream starts a session's stream; the core's Halt cancels it.
func (a *App) startStream(s video.Start) {
	if a.stopping() {
		return // the session will not start, and nothing is pushed
	}
	ctx, cancel := context.WithCancel(a.life)
	a.sessions[s.Session] = cancel
	a.away(func() {
		defer cancel()
		a.stream(ctx, s)
	})
}

// stream starts one session's stream, reads its bytes until the core halts it, then
// stops it.
func (a *App) stream(ctx context.Context, s video.Start) {
	st, ok := a.startOrFail(ctx, s)
	if !ok {
		return
	}
	defer st.Stop()
	a.follow(ctx, s.Session, st)
}

// startOrFail starts the stream, or tells the core why it could not.
func (a *App) startOrFail(ctx context.Context, s video.Start) (Stream, bool) {
	startCtx, cancel := context.WithTimeout(ctx, a.cfg.StartWait)
	st, err := a.deps.Streamer.Start(startCtx, s.Camera, s.Target)
	cancel()
	if err == nil {
		return st, true
	}
	if ctx.Err() != nil {
		return nil, false // halted meanwhile
	}
	why := video.RelayUnreachable
	if errors.Is(err, ErrCameraUnreachable) {
		why = video.CameraUnreachable
	}
	a.log.Warn("a video stream could not start", "session", s.Session, "camera", s.Camera, "err", err)
	a.submit(func(now time.Time) video.Input { return video.StreamFailed{Now: now, Session: s.Session, Reason: why} })
	return nil, false
}

// follow tells the core every ProgressEvery how many bytes the stream sent, until
// ctx is cancelled or the stream breaks.
func (a *App) follow(ctx context.Context, session string, st Stream) {
	for {
		fired, stop := a.deps.Clock.Timer(a.cfg.ProgressEvery)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-fired:
		}
		n, err := st.Sent(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			a.log.Warn("a video stream broke", "session", session, "err", err)
			a.submit(func(now time.Time) video.Input {
				return video.StreamFailed{Now: now, Session: session, Reason: video.RelayUnreachable}
			})
			return
		}
		a.submit(func(now time.Time) video.Input { return video.Progress{Now: now, Session: session, Bytes: n} })
	}
}

// checkCameras probes every camera, one after the other, away from the loop.
func (a *App) checkCameras() {
	if a.stopping() {
		return
	}
	a.away(func() {
		for _, c := range a.cfg.Video.Cameras {
			ctx, cancel := context.WithTimeout(a.life, a.cfg.ProbeWait)
			err := a.deps.Streamer.Probe(ctx, c.ID)
			cancel()
			if a.life.Err() != nil {
				return
			}
			if err != nil {
				a.log.Warn("a camera does not answer", "camera", c.ID, "err", err)
			}
			a.submit(func(now time.Time) video.Input { return video.Probe{Now: now, Camera: c.ID, Reachable: err == nil} })
		}
	})
}
