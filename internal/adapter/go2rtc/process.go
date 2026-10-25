package go2rtc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	stopGrace = 2 * time.Second
	// outputGrace bounds the wait for go2rtc's output once it exited: an ffmpeg it
	// started may still hold it.
	outputGrace = time.Second
)

// process is one go2rtc process, its private folder and its API.
type process struct {
	cmd     *exec.Cmd
	dir     string
	http    *http.Client
	exited  chan struct{} // closed once the process is gone and its output logged
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
	p := &process{dir: dir, http: unixClient(socket), exited: make(chan struct{}), secrets: secrets}
	if err := p.start(r.o.Binary, build(socket), log); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := p.waitReady(ctx); err != nil {
		p.stop()
		return nil, err
	}
	return p, nil
}

// unixClient talks HTTP to the API on its socket, whatever the URL's host.
func unixClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
}

// start writes the configuration, readable by the process alone, and starts go2rtc
// on it. What go2rtc prints goes to the logs, masked.
func (p *process) start(binary string, cfg map[string]any, log *slog.Logger) error {
	path := filepath.Join(p.dir, "go2rtc.json")
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err == nil {
		err = os.WriteFile(path, raw, 0o600)
	}
	if err != nil {
		return fmt.Errorf("go2rtc: %w", err)
	}
	// A pipe of our own rather than StdoutPipe: Wait must not close the output
	// before its last lines (often the reason it stopped) are read.
	output, sink := io.Pipe()
	cmd := exec.Command(binary, "-c", path)
	cmd.Dir = p.dir
	cmd.SysProcAttr = sysProcAttr()
	cmd.Stdout, cmd.Stderr = sink, sink
	cmd.WaitDelay = outputGrace
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("go2rtc: %w", err)
	}
	p.cmd = cmd
	logged := make(chan struct{})
	go func() { p.relayLogs(output, log); close(logged) }()
	go func() {
		cmd.Wait()
		sink.Close()
		<-logged
		close(p.exited)
	}()
	return nil
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
		// The target is also a query value of the request to go2rtc: an HTTP error
		// quotes that URL, where it is escaped once more, its token twice.
		out = append(out, target, url.QueryEscape(target))
		if u, err := url.Parse(target); err == nil {
			if tok := u.Query().Get("token"); tok != "" {
				out = append(out, tok, url.QueryEscape(tok), url.QueryEscape(url.QueryEscape(tok)))
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

// masked turns an error that may quote an address into one that does not.
func (p *process) masked(err error) error {
	return fmt.Errorf("go2rtc: %s", p.redact(err.Error()))
}

// excerpt is the start of what go2rtc answered, masked, to be shown in an error.
func (p *process) excerpt(body []byte) string {
	return p.redact(strings.TrimSpace(string(body[:min(len(body), 512)])))
}

// relayLogs logs what go2rtc prints, then drains what is left (a line too long for
// the scanner) so that go2rtc never blocks on its output.
func (p *process) relayLogs(r io.Reader, log *slog.Logger) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			log.Warn("go2rtc", "line", p.redact(line))
		}
	}
	io.Copy(io.Discard, r)
}

// call sends a request to the API. Its error is masked.
func (p *process) call(ctx context.Context, method, pathAndQuery string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://go2rtc"+pathAndQuery, nil)
	if err != nil {
		return nil, p.masked(err)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, p.masked(err)
	}
	return resp, nil
}

// waitReady waits for go2rtc to answer on its socket.
func (p *process) waitReady(ctx context.Context) error {
	for {
		if resp, err := p.call(ctx, http.MethodGet, "/api/streams"); err == nil {
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
