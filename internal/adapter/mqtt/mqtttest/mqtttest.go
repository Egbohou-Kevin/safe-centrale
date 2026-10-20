// Package mqtttest starts a private Mosquitto broker for a test. It is for tests
// only: shipped code must never import it (the architecture tests enforce that).
//
// The broker listens on 127.0.0.1 on a free port, keeps nothing on disk and is
// stopped when the test ends. If Mosquitto is not installed, the test is skipped,
// so the suite still runs on a machine without it.
package mqtttest

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Broker is a running private Mosquitto.
type Broker struct {
	t       testing.TB
	bin     string
	conf    string
	port    int
	wsPort  int // 0 without WebSockets
	tlsPort int // 0 without TLS
	wssPort int // 0 without both
	cmd     *exec.Cmd
}

// Options adds listeners to the plain one.
type Options struct {
	WebSockets bool      // MQTT in a WebSocket, at WSURL
	TLS        *TLSFiles // MQTT over TLS with these files, at TLSURL (and WSSURL with WebSockets)
}

// Start launches a broker and returns it. The test is skipped if there is no
// mosquitto binary on the PATH.
func Start(t testing.TB) *Broker {
	t.Helper()
	return StartWith(t, Options{})
}

// StartWith launches a broker with more listeners.
func StartWith(t testing.TB, o Options) *Broker {
	t.Helper()
	bin, err := exec.LookPath("mosquitto")
	if err != nil {
		t.Skip("mosquitto is not installed: skipping the test against a real broker")
	}
	b := &Broker{t: t, bin: bin, port: freePort(t)}
	text := fmt.Sprintf("per_listener_settings false\nallow_anonymous true\npersistence false\nlistener %d 127.0.0.1\n", b.port)
	if o.WebSockets {
		b.wsPort = freePort(t)
		text += fmt.Sprintf("listener %d 127.0.0.1\nprotocol websockets\n", b.wsPort)
	}
	if o.TLS != nil {
		b.tlsPort = freePort(t)
		text += fmt.Sprintf("listener %d 127.0.0.1\ncafile %s\ncertfile %s\nkeyfile %s\n", b.tlsPort, o.TLS.CA, o.TLS.Cert, o.TLS.Key)
		if o.WebSockets {
			b.wssPort = freePort(t)
			text += fmt.Sprintf("listener %d 127.0.0.1\nprotocol websockets\ncafile %s\ncertfile %s\nkeyfile %s\n", b.wssPort, o.TLS.CA, o.TLS.Cert, o.TLS.Key)
		}
	}
	b.conf = filepath.Join(t.TempDir(), "mosquitto.conf")
	if err := os.WriteFile(b.conf, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	b.Restart()
	t.Cleanup(b.Stop)
	return b
}

// URL is the address clients connect to.
func (b *Broker) URL() string { return fmt.Sprintf("mqtt://127.0.0.1:%d", b.port) }

// WSURL is the address of the WebSocket listener (StartWith, WebSockets).
func (b *Broker) WSURL() string { return fmt.Sprintf("ws://127.0.0.1:%d", b.wsPort) }

// TLSURL is the address of the TLS listener (StartWith, TLS). The certificate is
// for "localhost".
func (b *Broker) TLSURL() string { return fmt.Sprintf("mqtts://localhost:%d", b.tlsPort) }

// WSSURL is the address of the WebSocket listener over TLS (StartWith, both).
func (b *Broker) WSSURL() string { return fmt.Sprintf("wss://localhost:%d", b.wssPort) }

// Stop kills the broker. Clients see the connection drop.
func (b *Broker) Stop() {
	if b.cmd == nil {
		return
	}
	b.cmd.Process.Kill()
	b.cmd.Wait()
	b.cmd = nil
}

// Restart starts the broker again on the same port, with no memory of the
// previous run (no sessions, no retained messages).
func (b *Broker) Restart() {
	b.t.Helper()
	b.Stop()
	cmd := exec.Command(b.bin, "-c", b.conf)
	if err := cmd.Start(); err != nil {
		b.t.Fatalf("starting mosquitto: %v", err)
	}
	b.cmd = cmd

	for _, port := range []int{b.port, b.wsPort, b.tlsPort, b.wssPort} {
		if port != 0 {
			b.waitListening(port)
		}
	}
}

func (b *Broker) waitListening(port int) {
	b.t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("mosquitto did not start listening on %s", addr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
