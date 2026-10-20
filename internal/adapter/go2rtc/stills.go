package go2rtc

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
)

// stillsConfig is what the photos' process reads: the cameras, and the one API path
// that hands out a picture (and the list of streams, to know it answers).
func stillsConfig(socket, ffmpeg string, sources map[string]string) map[string]any {
	cfg := map[string]any{
		"app":     map[string]any{"modules": []string{"api", "rtsp", "mjpeg", "ffmpeg"}},
		"api":     map[string]any{"listen": "", "unix_listen": socket, "allow_paths": []string{"/api/streams", "/api/frame.jpeg"}},
		"rtsp":    map[string]any{"listen": ""},
		"log":     map[string]any{"level": "warn", "format": "text"},
		"streams": sources,
	}
	if ffmpeg != "" {
		cfg["ffmpeg"] = map[string]any{"bin": ffmpeg}
	}
	return cfg
}

// maxPicture bounds a picture: a reduced JPEG weighs tens of kilobytes.
const maxPicture = 4 << 20

// Stills is one go2rtc process ready to take pictures from every camera.
type Stills struct{ *process }

// OpenStills launches the photos' process and waits for it to answer. Close it once
// the pictures are taken.
func (r *Runner) OpenStills(ctx context.Context) (*Stills, error) {
	sources := slices.Collect(maps.Values(r.o.Sources))
	p, err := r.launch(ctx, func(socket string) map[string]any { return stillsConfig(socket, r.o.FFmpeg, r.o.Sources) },
		secrets(sources, ""), r.log.With("photos", true))
	if err != nil {
		return nil, err
	}
	return &Stills{process: p}, nil
}

// Frame returns a JPEG picture of the camera, width pixels wide. go2rtc waits for
// the camera's next key frame (a second or two) and has ffmpeg decode it. An error
// wrapping ErrCameraUnreachable says the camera gave no picture.
func (s *Stills) Frame(ctx context.Context, camera string, width int) ([]byte, error) {
	q := url.Values{"src": {camera}, "w": {strconv.Itoa(width)}}
	resp, err := s.call(ctx, http.MethodGet, "/api/frame.jpeg?"+q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPicture+1))
	if err != nil {
		return nil, s.masked(err)
	}
	switch {
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: no picture (%d) %q", ErrCameraUnreachable, resp.StatusCode, s.excerpt(body))
	case len(body) > maxPicture:
		return nil, fmt.Errorf("go2rtc: a picture larger than %d bytes", maxPicture)
	case len(body) < 3 || body[0] != 0xFF || body[1] != 0xD8:
		// go2rtc 1.9 answers 200 with its error as text when the camera fails.
		return nil, fmt.Errorf("%w: no picture %q", ErrCameraUnreachable, s.excerpt(body))
	}
	return body, nil
}

// Close ends the process and removes its folder.
func (s *Stills) Close() { s.stop() }
