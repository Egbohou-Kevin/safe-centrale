// Package mqtttest starts a private Mosquitto broker for a test. It is for tests
// only: shipped code must never import it (the architecture tests enforce that).
//
// The broker listens on 127.0.0.1 on a free port, keeps nothing on disk and is
// stopped when the test ends. If Mosquitto is not installed, the test is skipped,
// so the suite still runs on a machine without it.
//
// Start gives the hub's broker as a hub has it: no anonymous client, a password per
// name, and the rights of deploy/mosquitto/safehome.acl, read from the repository. A
// program under test connects with its own name, so a right forgotten in that file
// leaves it deaf and its test fails. StartOpen gives a broker that takes anyone on
// every topic: for the MQTT client's own tests and the cloud's side.
package mqtttest

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// ACL is the file of rights the hub's broker reads, from the repository's root.
var ACL = filepath.Join("deploy", "mosquitto", "safehome.acl")

// Peer is a name on a broker of Start that reads and writes every topic: the test
// clients that play the programs around the one under test use it. No hub has it: the
// test broker reads the shipped ACL with Peer's section added at its end.
const Peer = "mqtttest-peer"

// credentialsEnv and passwordCredential are where systemd puts a unit's password
// (internal/configfile reads it there).
const (
	credentialsEnv     = "CREDENTIALS_DIRECTORY"
	passwordCredential = "mqtt-password"
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

// Start launches the hub's broker (see the package) and returns it. program is the
// name the program under test has there ("safehome-hub", or "operator"): its password
// is put where systemd puts it (CREDENTIALS_DIRECTORY, for the test's duration), so the
// program connects as on a hub, with its rights alone. The test fails if the ACL has no
// section for program, and is skipped if there is no mosquitto binary on the PATH.
func Start(t testing.TB, program string) *Broker {
	t.Helper()
	bin := mosquitto(t)
	acl := readACL(t)
	names := Names(acl)
	if !slices.Contains(names, program) {
		t.Fatalf("%s has no section in %s", program, ACL)
	}
	dir := t.TempDir()
	aclFile := filepath.Join(dir, "safehome.acl")
	peer := fmt.Sprintf("\n# Added by mqtttest: the test's own clients.\nuser %s\ntopic readwrite #\n", Peer)
	if err := os.WriteFile(aclFile, append(acl, peer...), 0o600); err != nil {
		t.Fatal(err)
	}
	passwd := passwordFile(t, dir, append(names, Peer))
	creds := filepath.Join(dir, "credentials")
	if err := os.Mkdir(creds, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(creds, passwordCredential), []byte(Password(program)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(credentialsEnv, creds)
	return start(t, bin, fmt.Sprintf("allow_anonymous false\npassword_file %s\nacl_file %s\n", passwd, aclFile), Options{})
}

// StartOpen launches a broker that takes any client, with or without a name, on every
// topic. The test is skipped if there is no mosquitto binary on the PATH.
func StartOpen(t testing.TB) *Broker {
	t.Helper()
	return StartOpenWith(t, Options{})
}

// StartOpenWith launches an open broker (StartOpen) with more listeners.
func StartOpenWith(t testing.TB, o Options) *Broker {
	t.Helper()
	return start(t, mosquitto(t), "allow_anonymous true\n", o)
}

// Password is the password of name on a broker of Start, made for the test.
func Password(name string) string { return "test-password-of-" + name }

// Names returns the names of an ACL file's sections ("user NAME"), in their order.
func Names(acl []byte) []string {
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(acl))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[0] == "user" {
			names = append(names, f[1])
		}
	}
	return names
}

// readACL returns the shipped ACL, found from the test's folder up to the repository's
// root (the folder of go.mod).
func readACL(t testing.TB) []byte {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			b, err := os.ReadFile(filepath.Join(dir, ACL))
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		up := filepath.Dir(dir)
		if up == dir {
			t.Fatal(errors.New("no go.mod above the test's folder: the repository's ACL cannot be found"))
		}
		dir = up
	}
}

// passwordFile makes Mosquitto's file of names and hashed passwords, as install.sh
// does: a plain list hashed in place by mosquitto_passwd.
func passwordFile(t testing.TB, dir string, names []string) string {
	t.Helper()
	tool, err := exec.LookPath("mosquitto_passwd")
	if err != nil {
		t.Fatal("mosquitto is installed without mosquitto_passwd: the hub's broker cannot be made")
	}
	var plain strings.Builder
	for _, n := range names {
		fmt.Fprintf(&plain, "%s:%s\n", n, Password(n))
	}
	path := filepath.Join(dir, "safehome.passwd")
	if err := os.WriteFile(path, []byte(plain.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(tool, "-U", path).CombinedOutput(); err != nil {
		t.Fatalf("mosquitto_passwd: %v %s", err, out)
	}
	return path
}

func mosquitto(t testing.TB) string {
	t.Helper()
	bin, err := exec.LookPath("mosquitto")
	if err != nil {
		t.Skip("mosquitto is not installed: skipping the test against a real broker")
	}
	return bin
}

// start writes the configuration, security then listeners, and starts the broker.
func start(t testing.TB, bin, security string, o Options) *Broker {
	t.Helper()
	b := &Broker{t: t, bin: bin, port: freePort(t)}
	text := fmt.Sprintf("per_listener_settings false\n%spersistence false\nlistener %d 127.0.0.1\n", security, b.port)
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

// WSURL is the address of the WebSocket listener (StartOpenWith, WebSockets).
func (b *Broker) WSURL() string { return fmt.Sprintf("ws://127.0.0.1:%d", b.wsPort) }

// TLSURL is the address of the TLS listener (StartOpenWith, TLS). The certificate is
// for "localhost".
func (b *Broker) TLSURL() string { return fmt.Sprintf("mqtts://localhost:%d", b.tlsPort) }

// WSSURL is the address of the WebSocket listener over TLS (StartOpenWith, both).
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
