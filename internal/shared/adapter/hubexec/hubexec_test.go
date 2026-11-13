package hubexec

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// asHub makes the test binary a fake hub when it runs itself.
const asHub = "HUBEXEC_TEST_AS_HUB"

// The fake hub answers its arguments and its input, as told by its verb.
func TestMain(m *testing.M) {
	if os.Getenv(asHub) == "1" {
		a := os.Args[1:] // command -config FILE verb
		in, _ := io.ReadAll(os.Stdin)
		switch a[3] {
		case "echo":
			fmt.Printf("%s|%s|%s|%s|%s", a[0], a[2], in, os.Getenv("CREDENTIALS_DIRECTORY"), os.Getenv("HUBEXEC_KEPT"))
		case "refuse":
			fmt.Fprintln(os.Stderr, "  zone dev_x: unknown kind  ")
			os.Exit(1)
		case "silent":
			os.Exit(3)
		case "flood":
			fmt.Print(strings.Repeat("x", maxOutput+1))
		}
		os.Exit(0)
	}
	os.Setenv(asHub, "1")
	os.Exit(m.Run())
}

func fake(t *testing.T) Hub {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Hub{Program: self, Config: "/etc/safehome/hub.json"}
}

func TestCallGivesTheInputAndKeepsTheCredentialsOut(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "/run/credentials/safehome-console")
	t.Setenv("HUBEXEC_KEPT", "kept")
	out, err := fake(t).Call("site", "echo", []byte(`{"zones":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), `site|/etc/safehome/hub.json|{"zones":{}}||kept`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestARefusalIsTheHubsOwnWords(t *testing.T) {
	_, err := fake(t).Call("site", "refuse", nil)
	if err == nil || err.Error() != "zone dev_x: unknown kind" {
		t.Fatalf("refusal: %v", err)
	}
	if _, err := fake(t).Call("site", "silent", nil); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("a silent failure: %v", err)
	}
}

func TestTooLongAnAnswerIsRefused(t *testing.T) {
	if _, err := fake(t).Call("site", "flood", nil); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("flood: %v", err)
	}
}

func TestAMissingHubSaysWhere(t *testing.T) {
	h := Hub{Program: filepath.Join(t.TempDir(), "safehome-hub"), Config: "x"}
	if _, err := h.Call("site", "view", nil); err == nil || !strings.Contains(err.Error(), h.Program) {
		t.Fatalf("missing: %v", err)
	}
}

func TestBesideIsNextToTheRunningProgram(t *testing.T) {
	h, err := Beside("/etc/safehome/hub.json")
	self, _ := os.Executable()
	if err != nil || h.Program != filepath.Join(filepath.Dir(self), "safehome-hub") || h.Config != "/etc/safehome/hub.json" {
		t.Fatalf("%+v %v", h, err)
	}
}
