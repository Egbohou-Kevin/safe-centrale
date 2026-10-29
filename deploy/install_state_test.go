package deploy

import (
	"os"
	"slices"
	"strings"
	"testing"

	"safehome/internal/agentconfig"
)

// stateRig is an install rig with the state folder at hand.
type stateRig struct {
	*rig
	state string // the state folder, relative to the rig's root
}

func newStateRig(t *testing.T) stateRig {
	r := newRig(t)
	r.minimalSite()
	return stateRig{r, "var/lib/safehome/"}
}

func (r stateRig) read(f string) string {
	r.t.Helper()
	b, err := os.ReadFile(r.path(r.state + f))
	if err != nil {
		r.t.Errorf("%s: %v", f, err)
	}
	return string(b)
}

func (r stateRig) stat(f string) os.FileInfo {
	r.t.Helper()
	fi, err := os.Lstat(r.path(r.state + f))
	if err != nil {
		r.t.Fatalf("%s: %v", f, err)
	}
	return fi
}

// linked fails unless the flat file is a relative link to the folder's.
func (r stateRig) linked(folder, f string) {
	r.t.Helper()
	want := folder + "/" + f
	got, err := os.Readlink(r.path(r.state + f))
	if err != nil || got != want {
		r.t.Errorf("%s at the flat place: %q %v, want a link to %s (never a copy, never a hard link)", f, got, err, want)
	}
}

// A hub updated from before 29 October 2026: install.sh puts the agent's key in its
// folder and leaves at the flat place a relative link to it, the folder made in place
// before any unit restarts (an InaccessiblePaths= folder missing when a unit starts is
// not hidden); run again, nothing moves.
func TestInstallMovesTheAgentKeyToItsFolder(t *testing.T) {
	r := newStateRig(t)
	key, flat := agentconfig.KeyFolder+"/code-key", "code-key"
	writeFile(t, r.path(r.state+flat), "the key")

	cmds := r.mustRun()
	if got := r.read(key); got != "the key" {
		t.Errorf("%s = %q, want the key it had", key, got)
	}
	r.linked(agentconfig.KeyFolder, flat)
	if got := r.read(flat); got != "the key" {
		t.Errorf("%s read through the link = %q", flat, got)
	}
	if fi := r.stat(agentconfig.KeyFolder); !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("%s/: %v, want a folder 0700", agentconfig.KeyFolder, fi.Mode())
	}
	own := slices.Index(cmds, "chown safehome:safehome /"+r.state+agentconfig.KeyFolder)
	restart := slices.IndexFunc(cmds, func(c string) bool { return strings.HasPrefix(c, "systemctl restart safehome-") })
	if own < 0 || restart < 0 || own > restart {
		t.Errorf("the folder is not given to safehome before the units restart: %q", cmds)
	}
	if r.exists(r.state+flat+".new") || r.exists(r.state+key+".new") {
		t.Error("a file built aside stayed")
	}

	// Again: the folder and the key are the same files, the link the same.
	folder, kept := r.stat(agentconfig.KeyFolder), r.stat(key)
	r.mustRun()
	if !os.SameFile(folder, r.stat(agentconfig.KeyFolder)) {
		t.Error("the folder was replaced: a unit running keeps hiding the old one")
	}
	if !os.SameFile(kept, r.stat(key)) || r.read(key) != "the key" {
		t.Error("the key in the folder was copied again")
	}
	r.linked(agentconfig.KeyFolder, flat)
}

// The agent from 29 October 2026 keeps its key in its folder: when the flat place still
// holds a file (another key, never linked), the folder wins, filled in place, and the
// flat file becomes the link. One the folder alone holds gets the link too: an agent
// put back by a rollback reads the same key there, rather than drawing a new one.
func TestTheAgentFolderWinsOverAFlatKey(t *testing.T) {
	r := newStateRig(t)
	key, flat := agentconfig.KeyFolder+"/code-key", "code-key"
	writeFile(t, r.path(r.state+key), "the folder's key")
	writeFile(t, r.path(r.state+flat), "a flat key")
	folder := r.stat(agentconfig.KeyFolder)

	r.mustRun()
	if got := r.read(key); got != "the folder's key" {
		t.Errorf("%s = %q, want the folder's kept", key, got)
	}
	r.linked(agentconfig.KeyFolder, flat)
	if !os.SameFile(folder, r.stat(agentconfig.KeyFolder)) {
		t.Error("the folder was replaced: a unit running keeps hiding the old one")
	}

	if err := os.Remove(r.path(r.state + flat)); err != nil {
		t.Fatal(err)
	}
	r.mustRun()
	r.linked(agentconfig.KeyFolder, flat)
}

// Without a key anywhere, install.sh makes none (the agent draws it), nor a link to
// nothing, which an agent put back by a rollback would take for a lost key.
func TestNoAgentKeyLeavesNoLink(t *testing.T) {
	r := newStateRig(t)
	r.mustRun()
	if fi := r.stat(agentconfig.KeyFolder); !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("%s/: %v, want a folder 0700", agentconfig.KeyFolder, fi.Mode())
	}
	for _, f := range []string{"code-key", agentconfig.KeyFolder + "/code-key"} {
		if _, err := os.Lstat(r.path(r.state + f)); !os.IsNotExist(err) {
			t.Errorf("%s: %v, want nothing", f, err)
		}
	}
}

// A copy cut short (here a key it cannot read) leaves the flat key where it was, never
// a link to nothing; the next run makes the move.
func TestAnAgentKeyCopyCutShortLeavesTheFlatKey(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without rights")
	}
	r := newStateRig(t)
	key, flat := agentconfig.KeyFolder+"/code-key", "code-key"
	writeFile(t, r.path(r.state+flat), "the key")
	if err := os.Chmod(r.path(r.state+flat), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.run(); err == nil {
		t.Fatal("install.sh went on past a copy that failed")
	}
	if fi := r.stat(flat); !fi.Mode().IsRegular() {
		t.Errorf("%s: %v, want the flat key left", flat, fi.Mode())
	}
	if r.exists(r.state + key) {
		t.Errorf("a copy cut short left %s", key)
	}
	if err := os.Chmod(r.path(r.state+flat), 0o600); err != nil {
		t.Fatal(err)
	}
	r.mustRun()
	if got := r.read(key); got != "the key" {
		t.Errorf("%s = %q", key, got)
	}
	r.linked(agentconfig.KeyFolder, flat)
}
