// Package hubexec runs the hub's command line for the installer's tools: they ask
// the hub about its configuration (the contract is internal/shared/config/hubtool)
// without reading the hub's files themselves. It knows only the process: bounded in
// time and in output, without the caller's broker credentials.
package hubexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Bounds of one call: the hub reads its files and answers in a fraction of a second.
const (
	timeout   = 30 * time.Second
	maxOutput = 1 << 20
)

// Hub is the hub whose configuration file is Config, asked through Program.
type Hub struct {
	Program string // the hub's executable
	Config  string // its configuration file (/etc/safehome/hub.json)
}

// Beside is the hub installed next to the running program (/usr/local/bin).
func Beside(config string) (Hub, error) {
	self, err := os.Executable()
	if err != nil {
		return Hub{}, fmt.Errorf("hubexec: %w", err)
	}
	return Hub{Program: filepath.Join(filepath.Dir(self), "safehome-hub"), Config: config}, nil
}

// Call runs the hub's command (command -config FILE verb) with in on its standard
// input, and returns its standard output. A refusal is the hub's own words.
func (h Hub) Call(command, verb string, in []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.Program, command, "-config", h.Config, verb)
	cmd.Env = withoutCredentials(os.Environ())
	cmd.Stdin = bytes.NewReader(in)
	var stdout, stderr bounded
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("hubexec: the hub did not answer within %s", timeout)
	case errors.As(err, &exit):
		msg := strings.TrimSpace(stderr.buf.String())
		if msg == "" {
			msg = exit.String()
		}
		return nil, errors.New(msg)
	case err != nil:
		return nil, fmt.Errorf("hubexec: cannot run the hub (%s): %w", h.Program, err)
	case stdout.over || stderr.over:
		return nil, fmt.Errorf("hubexec: the hub's answer is over %d bytes", maxOutput)
	}
	return stdout.buf.Bytes(), nil
}

// withoutCredentials keeps the caller's own broker password out of the hub's
// process: the hub needs none to read its files.
func withoutCredentials(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "CREDENTIALS_DIRECTORY=") {
			out = append(out, kv)
		}
	}
	return out
}

// bounded keeps the first maxOutput bytes written, and says when there were more.
// It holds its buffer in a field: an embedded bytes.Buffer would give it ReadFrom,
// which io.Copy calls instead of Write, past the bound.
type bounded struct {
	buf  bytes.Buffer
	over bool
}

func (b *bounded) Write(p []byte) (int, error) {
	if room := maxOutput - b.buf.Len(); len(p) > room {
		b.over = true
		b.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return b.buf.Write(p)
}
