package go2rtc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const stream = "cam" // the one stream of a process

// config is what a stream's process reads: the API on its socket only, no RTSP
// server, no WebRTC, no web interface, and the camera.
func config(socket, source string) map[string]any {
	return map[string]any{
		"app":     map[string]any{"modules": []string{"api", "rtsp", "rtmp"}},
		"api":     map[string]any{"listen": "", "unix_listen": socket, "allow_paths": []string{"/api/streams"}},
		"rtsp":    map[string]any{"listen": ""},
		"log":     map[string]any{"level": "warn", "format": "text"},
		"streams": map[string]string{stream: source},
	}
}

// Stream is one go2rtc process pushing one camera.
type Stream struct {
	*process

	mu         sync.Mutex
	base, last int64 // go2rtc's counter restarts at each reconnection to the relay
}

// Start launches go2rtc for the camera, waits for it to answer, and asks it to push
// to target. It returns once the camera is connected; the connection to the relay
// is made afterwards (watch Sent).
func (r *Runner) Start(ctx context.Context, camera, target string) (*Stream, error) {
	source, ok := r.o.Sources[camera]
	if !ok {
		return nil, fmt.Errorf("go2rtc: unknown camera %q", camera)
	}
	p, err := r.launch(ctx, func(socket string) map[string]any { return config(socket, source) },
		secrets([]string{source}, target), r.log.With("camera", camera))
	if err != nil {
		return nil, err
	}
	s := &Stream{process: p}
	if err := s.publish(ctx, target); err != nil {
		s.Stop()
		return nil, err
	}
	return s, nil
}

// publish asks go2rtc to push the camera to target. go2rtc connects to the camera
// before answering, so its error says whether the camera answered.
func (s *Stream) publish(ctx context.Context, target string) error {
	q := url.Values{"src": {stream}, "dst": {target}}
	resp, err := s.call(ctx, http.MethodPost, "/api/streams?"+q.Encode())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	raw := strings.TrimSpace(string(body)) // read as it is; only masked to be shown
	msg := s.redact(raw)
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusInternalServerError && strings.HasPrefix(raw, "streams:"):
		if strings.Contains(raw, "codecs not matched") {
			return fmt.Errorf("%w: it does not send H.264 (%s)", ErrCameraUnreachable, msg)
		}
		if !strings.Contains(raw, "unsupported scheme") {
			return fmt.Errorf("%w: %s", ErrCameraUnreachable, msg)
		}
	}
	return fmt.Errorf("go2rtc: push refused (%d): %s", resp.StatusCode, msg)
}

// streamInfo is the part of GET /api/streams?src= that counts.
type streamInfo struct {
	Consumers []struct {
		BytesSend int64 `json:"bytes_send"`
	} `json:"consumers"`
}

// Sent returns the bytes pushed so far. go2rtc counts the video and audio it hands
// to the connection, not the RTMP and TLS framing (a few percent more on the wire).
func (s *Stream) Sent(ctx context.Context) (int64, error) {
	select {
	case <-s.exited:
		return s.total(), ErrExited
	default:
	}
	resp, err := s.call(ctx, http.MethodGet, "/api/streams?src="+stream)
	if err != nil {
		return s.total(), err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return s.total(), fmt.Errorf("go2rtc: stream status %d", resp.StatusCode)
	}
	var info streamInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return s.total(), fmt.Errorf("go2rtc: stream status: %w", err)
	}
	// The process listens to nobody: its only consumer is the push to the relay
	// (format "rtmp" in go2rtc 1.9.14), whatever go2rtc calls it.
	var now int64
	for _, c := range info.Consumers {
		now += c.BytesSend
	}
	return s.count(now), nil
}

// count adds go2rtc's counter to the total, which survives the counter's restarts.
func (s *Stream) count(now int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now < s.last {
		s.base += s.last
	}
	s.last = now
	return s.base + s.last
}

func (s *Stream) total() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base + s.last
}

// Stop ends the process and removes its folder.
func (s *Stream) Stop() { s.stop() }
