// Package go2rtc is the adapter between the video service and go2rtc
// (github.com/AlexxIT/go2rtc), the program that reads a camera's stream and pushes it
// to the cloud relay, without re-encoding it. It drives go2rtc through its HTTP API
// and knows nothing of sessions, allowances or the contract.
//
// For photos, one process reads every camera and hands out JPEG pictures, which
// go2rtc gets ffmpeg to decode (go2rtc itself never re-encodes a stream).
//
// It starts one go2rtc process per stream, because go2rtc's API has no way to stop a
// stream it pushes (it retries for ever): stopping the stream is stopping the
// process. Each process listens on nothing but a Unix socket in a private folder,
// where its configuration (the camera's address, password included) is written for
// it alone. go2rtc v1.9 pushes over RTMP and RTMPS only.
package go2rtc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
)

// ErrCameraUnreachable says the camera did not answer, or does not send what can be
// pushed (H.264).
var ErrCameraUnreachable = errors.New("go2rtc: camera unreachable")

// ErrExited says the go2rtc process of a stream is gone.
var ErrExited = errors.New("go2rtc: the process exited")

// Options configures a runner.
type Options struct {
	Binary  string            // the go2rtc executable
	Sources map[string]string // camera id -> its RTSP address, credentials included
	Logger  *slog.Logger      // never given an address with a credential
	// FFmpeg decodes a camera's picture into a JPEG (photos only); empty: "ffmpeg"
	// from the PATH.
	FFmpeg string
}

// Runner starts streams.
type Runner struct {
	o   Options
	log *slog.Logger
}

// New checks the options.
func New(o Options) (*Runner, error) {
	if o.Binary == "" {
		return nil, errors.New("go2rtc: Binary is required")
	}
	for id, src := range o.Sources {
		if _, err := hostPort(src); err != nil {
			return nil, fmt.Errorf("go2rtc: camera %s: %w", id, err)
		}
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{o: o, log: log}, nil
}

// hostPort returns where an RTSP camera listens. The error never repeats the
// address: it may hold a password.
func hostPort(src string) (string, error) {
	u, err := url.Parse(src)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("invalid address (rtsp://user:password@host/path)")
	}
	port := u.Port()
	switch {
	case u.Scheme == "rtsp" && port == "":
		port = "554"
	case u.Scheme == "rtsps" && port == "":
		port = "322"
	case u.Scheme != "rtsp" && u.Scheme != "rtsps":
		return "", fmt.Errorf("scheme %q: use rtsp:// or rtsps://", u.Scheme)
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

// Probe says whether the camera accepts a connection on its RTSP port.
func (r *Runner) Probe(ctx context.Context, camera string) error {
	src, ok := r.o.Sources[camera]
	if !ok {
		return fmt.Errorf("go2rtc: unknown camera %q", camera)
	}
	addr, _ := hostPort(src) // checked by New
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrCameraUnreachable, err)
	}
	return c.Close()
}
