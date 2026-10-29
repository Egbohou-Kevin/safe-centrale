package configfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type sample struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestDecodeBytesIsStrict(t *testing.T) {
	var s sample
	if err := DecodeBytes([]byte("{\"name\": \"a\", \"count\": 2}\n  \n"), &s); err != nil || s != (sample{"a", 2}) {
		t.Fatalf("%+v %v", s, err)
	}
	for name, body := range map[string]string{
		"broken":         `{"name": `,
		"unknown key":    `{"nmae": "a"}`,
		"wrong type":     `{"count": "two"}`,
		"empty":          ``,
		"second object":  `{"name": "a"} {}`,
		"stray brace":    `{"name": "a"} }`,
		"stray bracket":  `{"name": "a"}]`,
		"trailing words": `{"name": "a"} and more`,
	} {
		err := DecodeBytes([]byte(body), &sample{})
		if err == nil || !strings.HasPrefix(err.Error(), "invalid JSON: ") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := DecodeBytes([]byte(`{} {}`), &sample{}); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Errorf("trailing data is named: %v", err)
	}
}

func TestDecodeNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(good, []byte(`{"name": "a"}`), 0o600)
	os.WriteFile(bad, []byte(`{"name": 1}`), 0o600)
	var s sample
	if err := Decode(good, &s); err != nil || s.Name != "a" {
		t.Fatalf("%+v %v", s, err)
	}
	if err := Decode(bad, &s); err == nil || !strings.HasPrefix(err.Error(), bad+": invalid JSON") {
		t.Errorf("a file that does not decode: %v", err)
	}
	missing := filepath.Join(dir, "missing.json")
	if err := Decode(missing, &s); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), missing) {
		t.Errorf("a missing file: %v", err)
	}
}

func TestLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"": slog.LevelInfo, "info": slog.LevelInfo, "INFO": slog.LevelInfo, "debug": slog.LevelDebug,
		"warn": slog.LevelWarn, "warning": slog.LevelWarn, "error": slog.LevelError,
	} {
		if got, err := Level(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := Level("loud"); err == nil || !strings.Contains(err.Error(), `log_level "loud"`) {
		t.Errorf("%v", err)
	}
}

func TestDuration(t *testing.T) {
	d := time.Minute
	if err := Duration("wait", "", &d); err != nil || d != time.Minute {
		t.Fatalf("empty keeps the default: %v %v", d, err)
	}
	if err := Duration("wait", "1h30m", &d); err != nil || d != 90*time.Minute {
		t.Fatalf("%v %v", d, err)
	}
	err := Duration("alarm.wait", "soon", &d)
	if err == nil || !strings.Contains(err.Error(), `alarm.wait "soon"`) || !strings.Contains(err.Error(), "write for example") {
		t.Errorf("%v", err)
	}
	if d != 90*time.Minute {
		t.Errorf("a bad value changed the setting: %v", d)
	}
}

func TestPathAndSecret(t *testing.T) {
	dir := t.TempDir()
	if got := Path(dir, "a.secret"); got != filepath.Join(dir, "a.secret") {
		t.Errorf("relative: %s", got)
	}
	if got := Path(dir, "/etc/a.secret"); got != "/etc/a.secret" {
		t.Errorf("absolute: %s", got)
	}
	os.WriteFile(filepath.Join(dir, "a.secret"), []byte("  s3cret\n"), 0o600)
	if s, err := Secret(dir, "a.secret"); err != nil || s != "s3cret" {
		t.Errorf("%q %v", s, err)
	}
	if s, err := Secret(dir, filepath.Join(dir, "a.secret")); err != nil || s != "s3cret" {
		t.Errorf("absolute: %q %v", s, err)
	}
	if _, err := Secret(dir, "missing.secret"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%v", err)
	}
}

func TestWriteAtomicReplacesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	for _, content := range []string{"one", "two"} {
		if err := WriteAtomic(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(path); string(got) != content {
			t.Fatalf("got %q, want %q", got, content)
		}
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("a temporary file was left: %v", entries)
	}
	if err := WriteAtomic(path, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("perm %v", fi.Mode())
	}
}

func TestWriteAtomicFailsCleanly(t *testing.T) {
	if err := WriteAtomic(filepath.Join(t.TempDir(), "missing", "f.json"), []byte("x"), 0o600); err == nil {
		t.Error("a missing folder was accepted")
	}
	// A folder in the way of the rename: the write fails, and nothing is left.
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "f.json"), 0o700)
	os.WriteFile(filepath.Join(dir, "f.json", "keep"), nil, 0o600)
	if err := WriteAtomic(filepath.Join(dir, "f.json"), []byte("x"), 0o600); err == nil {
		t.Error("renamed over a folder")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("a temporary file was left: %v", entries)
	}
}

func TestProgramFileBuild(t *testing.T) {
	t.Setenv(credentialsEnv, "")
	var f struct {
		ProgramFile
		Other string `json:"other"`
	}
	if err := DecodeBytes([]byte(`{"site": "tg-lom-0001", "state_dir": "/s", "other": "x"}`), &f); err != nil {
		t.Fatal(err)
	}
	p, err := f.Build("safehome-gsm-", "where the gateway keeps its queue")
	if err != nil {
		t.Fatal(err)
	}
	if p != (Program{BrokerURL: "mqtt://127.0.0.1:1883", ClientID: "safehome-gsm-tg-lom-0001", Username: "safehome-gsm", StateDir: "/s", LogLevel: slog.LevelInfo}) {
		t.Fatalf("%+v", p)
	}
	f.BrokerURL, f.ClientID, f.LogLevel = "mqtt://10.0.0.1:1883", "mine", "debug"
	if p, err := f.Build("safehome-gsm-", "x"); err != nil || p.BrokerURL != f.BrokerURL || p.ClientID != "mine" || p.LogLevel != slog.LevelDebug {
		t.Fatalf("%+v %v", p, err)
	}
	f.LogLevel = "loud"
	if _, err := f.Build("safehome-gsm-", "x"); err == nil || !strings.Contains(err.Error(), "log_level") {
		t.Fatalf("%v", err)
	}
	f.StateDir = ""
	if _, err := f.Build("safehome-gsm-", "where the gateway keeps its queue"); err == nil ||
		err.Error() != "state_dir is required (where the gateway keeps its queue)" {
		t.Fatalf("%v", err)
	}
}

// The local broker's password comes from systemd's credentials folder, and shows
// nowhere: not in an error, not in a printed or logged configuration.
func TestTheBrokerPasswordComesFromTheCredentials(t *testing.T) {
	const secret = "pw-9f3Kq_x"
	f := ProgramFile{Site: "tg-lom-0001", StateDir: "/s"}

	t.Setenv(credentialsEnv, "")
	if p, err := f.Build("safehome-hub-", "x"); err != nil || p.Password != "" || p.Username != "safehome-hub" {
		t.Fatalf("without credentials: %+v %v", p, err)
	}

	dir := t.TempDir()
	t.Setenv(credentialsEnv, dir)
	path := filepath.Join(dir, PasswordCredential)
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := f.Build("safehome-hub-", "x")
	if err != nil || p.Password.Reveal() != secret {
		t.Fatalf("with credentials: %v", err)
	}
	url, id, user, pass := p.LocalBroker()
	if url != "mqtt://127.0.0.1:1883" || id != "safehome-hub-tg-lom-0001" || user != "safehome-hub" || pass != secret {
		t.Fatalf("LocalBroker: %s %s %s (password %t)", url, id, user, pass == secret)
	}
	var logged strings.Builder
	slog.New(slog.NewTextHandler(&logged, nil)).Info("starting", "cfg", p, "password", p.Password)
	for _, shown := range []string{fmt.Sprint(p), fmt.Sprintf("%+v %#v %q %x", p, p, p.Password, p.Password), logged.String(), string(must(json.Marshal(p)))} {
		if strings.Contains(shown, secret) {
			t.Errorf("the password shows: %s", shown)
		}
	}

	for name, setup := range map[string]func(){
		"unreadable": func() { os.Chmod(path, 0o000) },
		"missing":    func() { os.Remove(path) },
		"empty":      func() { os.WriteFile(path, []byte("\n"), 0o600) },
		"a folder":   func() { os.Remove(path); os.Mkdir(path, 0o700) },
	} {
		os.RemoveAll(path)
		os.WriteFile(path, []byte(secret), 0o600)
		setup()
		_, err := f.Build("safehome-hub-", "x")
		if name == "unreadable" && os.Getuid() == 0 {
			continue // root reads it anyway
		}
		if err == nil || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), secret) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A tool run by hand under sudo is the operator, with the password only root reads;
// on a computer without it, there is none.
func TestTheOperatorReadsItsPassword(t *testing.T) {
	t.Setenv(credentialsEnv, "")
	p, err := ProgramFile{Site: "tg-lom-0001", StateDir: "/s"}.Build("safehome-hub-", "x")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "operator.secret")
	if op, err := p.asOperator(path); err != nil || op.Username != "operator" || op.Password != "" {
		t.Fatalf("without the file: %+v %v", op, err)
	}
	os.WriteFile(path, []byte("op-secret\n"), 0o600)
	op, err := p.asOperator(path)
	if err != nil || op.Username != "operator" || op.Password.Reveal() != "op-secret" || op.ClientID != p.ClientID {
		t.Fatalf("%+v %v", op, err)
	}
	if os.Getuid() != 0 {
		os.Chmod(path, 0o000)
		if _, err := p.asOperator(path); err == nil || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "op-secret") {
			t.Errorf("unreadable: %v", err)
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
