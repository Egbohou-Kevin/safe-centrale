package agentapp

import (
	"context"
	"errors"
	"io"
	"time"

	"safehome/internal/agent"
	"safehome/internal/contract"
)

// Photos is the photos' folder, which the video service fills.
type Photos interface {
	// Open returns a photo and its size; an error wrapping ErrNoPhoto says it is gone.
	Open(id string) (io.ReadCloser, int64, error)
	Remove(id string) error
}

// Uploader sends a photo to the cloud over HTTPS.
type Uploader interface {
	// Put sends the photo to url with the token (never log it). An error wrapping
	// ErrUploadExpired says the address or the token were refused, one wrapping
	// ErrUploadRefused that the file was; any other is worth another try.
	Put(ctx context.Context, url, token string, body io.Reader, size int64) error
}

// The errors of the photos' ports.
var (
	ErrNoPhoto       = errors.New("agentapp: no such photo")
	ErrUploadExpired = errors.New("agentapp: upload address refused")
	ErrUploadRefused = errors.New("agentapp: photo refused")
)

// upload sends a photo away from the loop, and tells the core how it went.
func (a *App) upload(u agent.Upload) {
	a.uploads.Add(1)
	go func() {
		defer a.uploads.Done()
		result := a.put(u)
		a.submit(func(now time.Time) agent.Input { return agent.Uploaded{Now: now, Photo: u.Photo, Result: result} })
	}()
}

func (a *App) put(u agent.Upload) agent.UploadResult {
	f, size, err := a.deps.Photos.Open(u.Photo)
	if errors.Is(err, ErrNoPhoto) {
		return agent.UploadMissing
	}
	if err != nil {
		a.log.Warn("a photo could not be read", "photo", u.Photo, "err", err)
		return agent.UploadRetry
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(a.life, a.cfg.PhotoWait)
	defer cancel()
	err = a.deps.Uploader.Put(ctx, u.URL, u.Token, f, size)
	switch {
	case err == nil:
		return agent.UploadOK
	case errors.Is(err, ErrUploadExpired):
		return agent.UploadExpired
	case errors.Is(err, ErrUploadRefused):
		a.log.Warn("the cloud refused a photo", "photo", u.Photo, "err", err)
		return agent.UploadRefused
	}
	if a.life.Err() == nil {
		a.log.Warn("a photo could not be sent, trying again later", "photo", u.Photo, "err", err)
	}
	return agent.UploadRetry
}

// dispatchPhoto carries out what the core decided about a photo.
func (a *App) dispatchPhoto(now time.Time, act agent.Action) {
	switch v := act.(type) {
	case agent.Upload:
		a.upload(v)
	case agent.RemovePhoto:
		if err := a.deps.Photos.Remove(v.Photo); err != nil {
			a.log.Warn("a photo could not be removed", "photo", v.Photo, "err", err)
		}
	case agent.PhotoReport:
		if v.Status == contract.PhotoSent {
			a.log.Info("photo sent to the cloud", "photo", v.Photo)
		} else {
			a.log.Warn("photo not sent", "photo", v.Photo, "reason", v.Reason)
		}
		a.publishOwn(now, contract.PhotoSentTopic(a.cfg.Site), contract.TypePhotoSent,
			contract.PhotoSentData{Photo: v.Photo, Status: v.Status, Reason: v.Reason}, false)
	}
}
