package videoapp

import (
	"context"
	"time"

	"safehome/internal/contract"
	"safehome/internal/video"
)

// takePhotos takes an alarm's photos away from the loop.
func (a *App) takePhotos(tp video.TakePhotos) {
	if a.stopping() {
		return
	}
	a.away(func() { a.shoot(tp) })
}

// shoot takes Count rounds of pictures Every apart, one of each camera per round. A
// camera that gives no picture is left out of the next rounds. Each photo is kept,
// then announced; their bytes count in the month's usage.
func (a *App) shoot(tp video.TakePhotos) {
	ctx, cancel := context.WithTimeout(a.base, a.cfg.PhotoWait)
	sh, err := a.deps.Camera.Open(ctx)
	cancel()
	if err != nil {
		a.log.Warn("the photos of the alarm could not be taken", "incident", tp.Incident, "err", err)
		return
	}
	defer sh.Close()
	var taken int
	var bytes int64
	failed := map[string]bool{}
	for round := range tp.Count {
		if round > 0 && !a.pause(tp.Every) {
			return
		}
		for _, cam := range tp.Cameras {
			if failed[cam] {
				continue
			}
			if a.base.Err() != nil {
				return
			}
			n, ok := a.photograph(sh, tp, cam)
			failed[cam] = !ok
			if n > 0 {
				taken++
				bytes += n
			}
		}
	}
	a.log.Info("photos of the alarm taken", "incident", tp.Incident, "photos", taken, "bytes", bytes)
	if bytes > 0 {
		a.submit(func(now time.Time) video.Input { return video.PhotosTaken{Now: now, Bytes: bytes} })
	}
}

// pause waits d, and reports false if the service stops meanwhile.
func (a *App) pause(d time.Duration) bool {
	fired, stop := a.deps.Clock.Timer(d)
	select {
	case <-a.base.Done():
		stop()
		return false
	case <-fired:
		return true
	}
}

// photograph takes one picture of the camera, keeps it and announces it. It returns
// the picture's size, 0 if it could not be kept, and false if the camera gave none.
func (a *App) photograph(sh Shooter, tp video.TakePhotos, cam string) (int64, bool) {
	ctx, cancel := context.WithTimeout(a.base, a.cfg.PhotoWait)
	jpeg, err := sh.Frame(ctx, cam, tp.Width)
	cancel()
	if err != nil {
		a.log.Warn("a camera gave no photo", "incident", tp.Incident, "camera", cam, "err", err)
		return 0, false
	}
	id := a.deps.NewID()
	if err := a.deps.Photos.Save(id, jpeg); err != nil {
		a.log.Error("a photo could not be kept", "incident", tp.Incident, "camera", cam, "err", err)
		return 0, true
	}
	size := int64(len(jpeg))
	a.enqueue(contract.PhotoTakenTopic(a.cfg.Site), contract.TypePhotoTaken, contract.PhotoTakenData{
		Photo: id, Incident: tp.Incident, Camera: cam, Bytes: size, Width: tp.Width,
	}, false, a.deps.Clock.Now())
	return size, true
}
