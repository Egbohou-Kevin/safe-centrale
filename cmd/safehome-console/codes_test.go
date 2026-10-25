package main

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"safehome/internal/adapter/filestore"
	"safehome/internal/adapter/pidfile"
	"safehome/internal/consoleapp"
	"safehome/internal/driver/drivertest"
	"safehome/internal/escalation"
	"safehome/internal/hub"
	"safehome/internal/hubconfig"
)

func TestCodesGoToTheHubsFilesAndTheHubIsTold(t *testing.T) {
	_, hubPath, _ := setup(t, "mqtt://127.0.0.1:1883", 80)
	cfg, err := hubconfig.Load(hubPath)
	if err != nil {
		t.Fatal(err)
	}
	logs := drivertest.Logs(t)
	c := codes{hubConfig: hubPath, log: slog.New(slog.NewTextHandler(logs, nil))}

	people, leftovers, err := c.People()
	if err != nil || len(leftovers) != 0 || len(people) != 4 || people[0].ID != "u_owner" || people[0].Role != "owner" || people[0].HasCode {
		t.Fatalf("%+v %v", people, err)
	}

	store, err := filestore.Open(cfg.StateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := hub.OpenSiteCodes(cfg.Hub, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := tool.Set(hub.InstallerUser, "3691"); err != nil {
		t.Fatal(err)
	}
	for code, want := range map[string]error{
		"3691": consoleapp.ErrCodeTaken, // the installer's: one code opens one thing
		"1234": consoleapp.ErrCodeWeak,
		"12":   consoleapp.ErrCodeInvalid,
	} {
		if err := c.Set("u_owner", code); !errors.Is(err, want) {
			t.Errorf("%s: %v", code, err)
		}
	}

	// A running hub is told to read the codes again (SIGHUP): a stand-in process here.
	stand := exec.Command("sleep", "30")
	if err := stand.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stand.Process.Kill() })
	running, err := pidfile.Lock(cfg.StateDir, hubconfig.PidFile) // a running hub holds its pid file
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cfg.StateDir, hubconfig.PidFile), []byte(strconv.Itoa(stand.Process.Pid)+"\n"), 0o600)
	if err := c.Set("u_owner", "2580"); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- stand.Wait() }()
	select {
	case err := <-waited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGHUP {
			t.Fatalf("the hub was not sent SIGHUP: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hub was not told")
	}
	running() // the stand-in hub is gone

	people, _, _ = c.People()
	if !people[0].HasCode {
		t.Fatalf("%+v", people[0])
	}

	// A code left by someone no longer a contact is listed apart, and removed.
	before := cfg.Hub
	before.Escalation.Contacts = append(slices.Clone(before.Escalation.Contacts), escalation.Contact{ID: "u_gone"})
	if other, err := hub.OpenSiteCodes(before, store); err != nil || other.Set("u_gone", "7391") != nil {
		t.Fatal(err)
	}
	if _, leftovers, err := c.People(); err != nil || !slices.Equal(leftovers, []string{"u_gone"}) {
		t.Fatalf("%v %v", leftovers, err)
	}
	if err := c.Remove("u_gone"); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove("u_gone"); !errors.Is(err, consoleapp.ErrNoSuchCode) {
		t.Fatalf("%v", err)
	}
	if _, leftovers, _ := c.People(); len(leftovers) != 0 {
		t.Fatalf("%v", leftovers)
	}
	if err := c.Set(people[1].ID, "2580"); !errors.Is(err, consoleapp.ErrCodeTaken) {
		t.Fatalf("the hub's files do not hold the code: %v", err)
	}
	if err := c.Set("u_stranger", "2468"); !errors.Is(err, consoleapp.ErrNotAContact) {
		t.Fatalf("a code for someone not a contact: %v", err)
	}
	// The installer's code is not the console's: a form posted by hand with
	// id=installer neither replaces nor removes it.
	if err := c.Set(hub.InstallerUser, "1593"); !errors.Is(err, consoleapp.ErrNotAContact) {
		t.Fatalf("the installer's code replaced from the console: %v", err)
	}
	if err := c.Remove(hub.InstallerUser); !errors.Is(err, consoleapp.ErrNoSuchCode) {
		t.Fatalf("the installer's code removed from the console: %v", err)
	}
	if err := c.Set("u_owner", "3691"); !errors.Is(err, consoleapp.ErrCodeTaken) {
		t.Fatalf("the installer's code must still be 3691: %v", err)
	}
	if strings.Contains(logs.String(), "2580") {
		t.Fatal("the code shows in the logs")
	}
}

// Two people set their codes at the same moment, from two phones: each change reads
// the list, hashes for seconds, and writes the whole list. Both codes are kept, as
// both are told; a code set by the command-line tool meanwhile too.
func TestCodesSetAtOnceAreAllKept(t *testing.T) {
	_, hubPath, _ := setup(t, "mqtt://127.0.0.1:1883", 80)
	c := codes{hubConfig: hubPath, log: slog.New(slog.DiscardHandler)}
	errs := make(chan error, 2)
	for user, code := range map[string]string{"u_owner": "2580", "u_deputy": "3917"} {
		go func() { errs <- c.Set(user, code) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	people, _, err := c.People()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range people {
		if (p.ID == "u_owner" || p.ID == "u_deputy") && !p.HasCode {
			t.Fatalf("%s's code was lost: %+v", p.ID, people)
		}
	}
}
