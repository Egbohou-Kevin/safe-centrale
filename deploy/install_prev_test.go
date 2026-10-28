package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// install.sh keeps the way back for update-hub.sh: the programs it replaces as X.prev, and
// the mark that names its run while they are not confirmed (docs/systeme.md, "Maintenance").

// An update keeps the program it replaces as X.prev, for update-hub.sh to put back when
// the services do not hold; an unchanged or new program leaves none, not even one left
// from an earlier update.
func TestAnUpdateKeepsThePreviousProgramsOnlyWhereTheyChanged(t *testing.T) {
	r := newRig(t)
	r.minimalSite()
	r.mustRun()
	bin := "usr/local/bin/"
	if matches, _ := filepath.Glob(r.path(bin + "*.prev")); len(matches) > 0 {
		t.Fatalf("a first install kept %v", matches)
	}
	running, _ := os.ReadFile(r.path(bin + "safehome-hub"))
	writeScript(t, filepath.Join(r.bundle, "bin", "safehome-hub"), "# the new hub\n"+fakeProgram)
	writeFile(t, r.path(bin+"safehome-gsm.prev"), "left from an earlier update")
	if err := os.Remove(r.path(bin + "safehome-power")); err != nil {
		t.Fatal(err)
	}

	r.mustRun()
	if b, err := os.ReadFile(r.path(bin + "safehome-hub.prev")); err != nil || string(b) != string(running) {
		t.Errorf("safehome-hub.prev = %q (%v), want the program it replaced", b, err)
	}
	if b, _ := os.ReadFile(r.path(bin + "safehome-hub")); !strings.Contains(string(b), "# the new hub") {
		t.Error("safehome-hub is not the new program")
	}
	for _, p := range []string{"safehome-gsm.prev", "safehome-power.prev", "safehome-agent.prev", "safehome-install.prev"} {
		if r.exists(bin + p) {
			t.Errorf("%s kept, though its program is unchanged or new", p)
		}
	}
}

// Until phase 2 confirms the new programs (it removes the mark), running install.sh again
// keeps the .prev it made: they are the programs that held, the only way back. Once
// confirmed, the next install is an update like any other.
func TestARunAgainBeforeConfirmationKeepsThePreviousPrograms(t *testing.T) {
	r := newRig(t)
	r.minimalSite()
	r.mustRun()
	bin := "usr/local/bin/"
	if r.exists(unconfirmed) {
		t.Fatal("a first install, which keeps no .prev, left the mark")
	}
	held := map[string][]byte{}
	for _, p := range []string{"safehome-hub", "safehome-agent"} {
		held[p] = must(os.ReadFile(r.path(bin + p)))
	}
	writeScript(t, filepath.Join(r.bundle, "bin", "safehome-hub"), "# the new hub\n"+fakeProgram)
	r.mustRun()
	if !r.exists(unconfirmed) {
		t.Fatal("an update that kept a .prev left no mark")
	}
	r.mustRun() // phase 2 run again after an interruption
	writeScript(t, filepath.Join(r.bundle, "bin", "safehome-agent"), "# the new agent\n"+fakeProgram)
	r.mustRun() // and with a bundle changed in between
	for p, want := range held {
		if b, err := os.ReadFile(r.path(bin + p + ".prev")); err != nil || string(b) != string(want) {
			t.Errorf("%s.prev = %q (%v), want the program that held", p, b, err)
		}
	}

	if err := os.Remove(r.path(unconfirmed)); err != nil { // phase 2 confirmed them
		t.Fatal(err)
	}
	r.mustRun()
	for _, p := range []string{"safehome-hub.prev", "safehome-agent.prev"} {
		if r.exists(bin + p) {
			t.Errorf("%s kept after the confirmation, though its program is unchanged", p)
		}
	}
	if r.exists(unconfirmed) {
		t.Error("an install that kept no .prev left the mark")
	}
}

// A run cut short while it keeps a .prev leaves no .prev cut short, nor one left from an
// earlier update for a program it had not reached yet: the next run, which keeps the .prev
// it finds, would put back a broken program, or one older than the program that held.
func TestARunCutShortLeavesNoBadWayBack(t *testing.T) {
	r := newRig(t)
	r.minimalSite()
	r.mustRun()
	bin := "usr/local/bin/"
	held := must(os.ReadFile(r.path(bin + "safehome-hub")))
	writeFile(t, r.path(bin+"safehome-power.prev"), "left from an earlier, confirmed update")
	writeScript(t, filepath.Join(r.bundle, "bin", "safehome-hub"), "# the new hub\n"+fakeProgram)
	cut := filepath.Join(filepath.Dir(r.root), "stubs", "cp")
	writeScript(t, cut, `case "$*" in *safehome-hub.prev*) for a; do :; done; head -c 3 "$2" > "$a"; exit 1 ;; esac; exec /bin/cp "$@"`)
	if out, _, err := r.run(); err == nil {
		t.Fatalf("install.sh went on after its copy was cut:\n%s", out)
	}
	if err := os.Remove(cut); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(r.path(unconfirmed))
	if err != nil || len(first) == 0 {
		t.Fatalf("the run cut short left no mark naming it: %q (%v)", first, err)
	}

	r.mustRun() // phase 2 run again
	if b, err := os.ReadFile(r.path(bin + "safehome-hub.prev")); err != nil || string(b) != string(held) {
		t.Errorf("safehome-hub.prev = %q (%v), want the program that held", b, err)
	}
	for _, p := range []string{"safehome-power.prev", "safehome-hub.prev.new"} {
		if r.exists(bin + p) {
			t.Errorf("%s left: never a way back", p)
		}
	}
	if again, _ := os.ReadFile(r.path(unconfirmed)); len(again) == 0 || string(again) == string(first) {
		t.Errorf("mark %q after %q: each run names itself", again, first)
	}
}

// The mark is readable by all (0644), even when an earlier one, which > keeps the mode
// of, was not: update-hub.sh reads it, and an unreadable mark tells nothing.
func TestTheMarkIsReadableByAll(t *testing.T) {
	r := newRig(t)
	r.minimalSite()
	r.mustRun()
	writeFile(t, r.path(unconfirmed), "left by an earlier run")
	if err := os.Chmod(r.path(unconfirmed), 0o600); err != nil {
		t.Fatal(err)
	}
	writeScript(t, filepath.Join(r.bundle, "bin", "safehome-hub"), "# the new hub\n"+fakeProgram)
	r.mustRun()
	fi, err := os.Stat(r.path(unconfirmed))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("mark %v (%v), want -rw-r--r--", fi, err)
	}
	if b, _ := os.ReadFile(r.path(unconfirmed)); string(b) == "left by an earlier run" {
		t.Error("the mark does not name this run")
	}
}
