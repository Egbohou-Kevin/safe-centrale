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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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

const (
	stream    = "cam" // the one stream of a process
	stopGrace = 2 * time.Second
)

// New checks the options.
func New(o Options) (*Runner, error) {
	if o.Binary == "" {
		return nil, errors.New("go2rtc: Binary is required")
	}
	for id, src := range o.Sources {
		if _, _, err := hostPort(src); err != nil {
			return nil, fmt.Errorf("go2rtc: camera %s: %w", id, err)
		}
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{o: o, log: log}, nil
}

// hostPort returns where an RTSP camera listens.
func hostPort(src string) (*url.URL, string, error) {
	u, err := url.Parse(src)
	if err != nil || u.Hostname() == "" {
		return nil, "", errors.New("invalid address (rtsp://user:password@host/path)")
	}
	port := u.Port()
	switch {
	case u.Scheme == "rtsp" && port == "":
		port = "554"
	case u.Scheme == "rtsps" && port == "":
		port = "322"
	case u.Scheme != "rtsp" && u.Scheme != "rtsps":
		return nil, "", fmt.Errorf("scheme %q: use rtsp:// or rtsps://", u.Scheme)
	}
	return u, net.JoinHostPort(u.Hostname(), port), nil
}

// Probe says whether the camera accepts a connection on its RTSP port.
func (r *Runner) Probe(ctx context.Context, camera string) error {
	src, ok := r.o.Sources[camera]
	if !ok {
		return fmt.Errorf("go2rtc: unknown camera %q", camera)
	}
	_, addr, _ := hostPort(src)
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrCameraUnreachable, err)
	}
	return c.Close()
}

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

// process is one go2rtc process, its private folder and its API.
type process struct {
	cmd     *exec.Cmd
	dir     string
	http    *http.Client
	exited  chan struct{}
	secrets []string

	stopOnce sync.Once
}

// launch starts go2rtc with the configuration built for its socket, and waits for
// it to answer.
func (r *Runner) launch(ctx context.Context, build func(socket string) map[string]any, secrets []string, log *slog.Logger) (*process, error) {
	dir, err := os.MkdirTemp("", "safehome-go2rtc-")
	if err != nil {
		return nil, fmt.Errorf("go2rtc: %w", err)
	}
	socket := filepath.Join(dir, "api.sock")
	cfg, err := json.MarshalIndent(build(socket), "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "go2rtc.json"), cfg, 0o600)
	}
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("go2rtc: %w", err)
	}
	p := &process{dir: dir, exited: make(chan struct{}), secrets: secrets}
	p.http = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	cmd := exec.Command(r.o.Binary, "-c", filepath.Join(dir, "go2rtc.json"))
	cmd.Dir = dir
	cmd.SysProcAttr = sysProcAttr()
	out, err := cmd.StdoutPipe()
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("go2rtc: %w", err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("go2rtc: %w", err)
	}
	p.cmd = cmd
	go p.relayLogs(out, log)
	go func() { cmd.Wait(); close(p.exited) }()
	if err := p.waitReady(ctx); err != nil {
		p.stop()
		return nil, err
	}
	return p, nil
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

// secrets lists what must never reach a log: the addresses, the cameras' passwords,
// the relay's token.
func secrets(sources []string, target string) []string {
	var out []string
	for _, source := range sources {
		out = append(out, source)
		if u, err := url.Parse(source); err == nil {
			if pw, ok := u.User.Password(); ok && pw != "" {
				out = append(out, pw, url.QueryEscape(pw), url.PathEscape(pw))
			}
		}
	}
	if target != "" {
		out = append(out, target)
		if u, err := url.Parse(target); err == nil {
			if tok := u.Query().Get("token"); tok != "" {
				out = append(out, tok, url.QueryEscape(tok))
			}
		}
	}
	return out
}

// redact masks every secret in text.
func (p *process) redact(text string) string {
	for _, secret := range p.secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "***")
		}
	}
	return text
}

func (p *process) relayLogs(r io.Reader, log *slog.Logger) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			log.Warn("go2rtc", "line", p.redact(line))
		}
	}
}

// waitReady waits for go2rtc to answer on its socket.
func (p *process) waitReady(ctx context.Context) error {
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://go2rtc/api/streams", nil)
		resp, err := p.http.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("go2rtc: did not answer: %w", ctx.Err())
		case <-p.exited:
			return fmt.Errorf("go2rtc: did not start: %w", ErrExited)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// publish asks go2rtc to push the camera to target. go2rtc connects to the camera
// before answering, so its error says whether the camera answered.
func (s *Stream) publish(ctx context.Context, target string) error {
	q := url.Values{"src": {stream}, "dst": {target}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://go2rtc/api/streams?"+q.Encode(), nil)
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("go2rtc: %s", s.redact(err.Error()))
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
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://go2rtc/api/streams?src="+stream, nil)
	resp, err := s.http.Do(req)
	if err != nil {
		return s.total(), fmt.Errorf("go2rtc: %s", s.redact(err.Error()))
	}
	defer resp.Body.Close()
	var info streamInfo
	if resp.StatusCode != http.StatusOK {
		return s.total(), fmt.Errorf("go2rtc: stream status %d", resp.StatusCode)
	}
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

// stop ends the process (politely, then not) and removes its folder.
func (p *process) stop() {
	p.stopOnce.Do(func() {
		p.cmd.Process.Signal(os.Interrupt)
		select {
		case <-p.exited:
		case <-time.After(stopGrace):
			p.cmd.Process.Kill()
			<-p.exited
		}
		p.http.CloseIdleConnections()
		os.RemoveAll(p.dir)
	})
}

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
	var sources []string
	for _, src := range r.o.Sources {
		sources = append(sources, src)
	}
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
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://go2rtc/api/frame.jpeg?"+q.Encode(), nil)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("go2rtc: %s", s.redact(err.Error()))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPicture+1))
	if err != nil {
		return nil, fmt.Errorf("go2rtc: %s", s.redact(err.Error()))
	}
	switch {
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: no picture (%d) %q", ErrCameraUnreachable, resp.StatusCode, s.redact(strings.TrimSpace(string(body[:min(len(body), 512)]))))
	case len(body) > maxPicture:
		return nil, fmt.Errorf("go2rtc: a picture larger than %d bytes", maxPicture)
	case len(body) < 3 || body[0] != 0xFF || body[1] != 0xD8:
		// go2rtc 1.9 answers 200 with its error as text when the camera fails.
		return nil, fmt.Errorf("%w: no picture %q", ErrCameraUnreachable, s.redact(strings.TrimSpace(string(body[:min(len(body), 512)]))))
	}
	return body, nil
}

// Close ends the process and removes its folder.
func (s *Stills) Close() { s.stop() }
