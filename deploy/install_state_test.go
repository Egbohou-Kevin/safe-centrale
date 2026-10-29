package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"safehome/internal/agentconfig"
	"safehome/internal/hubconfig"
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

// codeDocs are the access codes' documents (hub, console, codes tool), each with its
// previous version (filestore): install.sh moves the six files, never codes.lock.
var codeDocs = []string{"access.json", "installer.json", "codes-taken.json"}

// codesMark is the mark install.sh leaves in the codes' folder once every link is there.
func codesMark(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^CODES_MARK="([^"/]+)"$`).FindSubmatch(b)
	if m == nil {
		t.Fatal(`install.sh: no CODES_MARK="..."`)
	}
	return hubconfig.CodesFolder + "/" + string(m[1])
}

// runWatchingStops runs install.sh with a systemctl that notes, at each stop, whether
// access.json is still a flat file then.
func (r stateRig) runWatchingStops() []string {
	r.t.Helper()
	stubs := filepath.Join(r.t.TempDir(), "stubs")
	writeScript(r.t, filepath.Join(stubs, "systemctl"), `echo "systemctl $*" >> "$LOG"
if [ "$1" = stop ]; then
	if [ -L "$ROOT/`+r.state+`access.json" ]; then echo "at the stop: access.json linked" >> "$LOG"
	else echo "at the stop: access.json flat" >> "$LOG"; fi
fi`)
	path := ""
	for _, e := range r.env {
		if p, ok := strings.CutPrefix(e, "PATH="); ok {
			path = p
		}
	}
	return r.mustRun("PATH=" + stubs + string(os.PathListSeparator) + path)
}

// A hub updated from before the codes' folder: install.sh stops the console and the hub
// (neither writes a document between the choice and the copy), copies the six files into
// codes/, leaves relative links at the flat places, then the mark, and the hub starts
// again. codes.lock and hub.pid stay where they are. Run again, nothing moves and nothing
// stops.
func TestInstallMovesTheCodesToTheirFolder(t *testing.T) {
	r := newStateRig(t)
	for _, d := range codeDocs {
		writeFile(t, r.path(r.state+d), "flat "+d)
		writeFile(t, r.path(r.state+d+".prev"), "flat "+d+".prev")
	}
	writeFile(t, r.path(r.state+hubconfig.CodesLock), "")
	writeFile(t, r.path(r.state+hubconfig.PidFile), "42")

	cmds := r.runWatchingStops()
	for _, d := range codeDocs {
		for _, f := range []string{d, d + ".prev"} {
			if got := r.read(hubconfig.CodesFolder + "/" + f); got != "flat "+f {
				t.Errorf("%s/%s = %q, want what the flat one held", hubconfig.CodesFolder, f, got)
			}
			r.linked(hubconfig.CodesFolder, f)
			if r.exists(r.state+f+".new") || r.exists(r.state+hubconfig.CodesFolder+"/"+f+".new") {
				t.Errorf("%s: a file built aside stayed", f)
			}
		}
	}
	for _, f := range []string{hubconfig.CodesLock, hubconfig.PidFile} {
		if fi := r.stat(f); !fi.Mode().IsRegular() {
			t.Errorf("%s: %v, want the flat file left as it is", f, fi.Mode())
		}
	}
	if r.exists(r.state + hubconfig.CodesFolder + "/" + hubconfig.CodesLock) {
		t.Errorf("%s copied into %s/: it lives there alone, made by the tools", hubconfig.CodesLock, hubconfig.CodesFolder)
	}
	if fi := r.stat(hubconfig.CodesFolder); !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("%s/: %v, want a folder 0700", hubconfig.CodesFolder, fi.Mode())
	}
	if !r.exists(r.state + codesMark(t)) {
		t.Errorf("no %s once every link is there", codesMark(t))
	}
	own := slices.Index(cmds, "chown safehome:safehome /"+r.state+hubconfig.CodesFolder)
	restart := slices.Index(cmds, "systemctl restart safehome-hub")
	if own < 0 || restart < 0 || own > restart {
		t.Errorf("the folder is not given to safehome before the hub restarts: %q", cmds)
	}
	for _, p := range []string{"console", "hub"} {
		stop := slices.Index(cmds, "systemctl stop safehome-"+p)
		if stop < 0 || stop > restart || stop+1 >= len(cmds) || cmds[stop+1] != "at the stop: access.json flat" {
			t.Errorf("safehome-%s not stopped before the codes move: %q", p, cmds)
		}
	}

	// Again: the folder and the documents are the same files, the links the same, and
	// neither the console nor the hub stops for nothing.
	folder, kept := r.stat(hubconfig.CodesFolder), r.stat(hubconfig.CodesFolder+"/access.json")
	cmds = r.runWatchingStops()
	if !os.SameFile(folder, r.stat(hubconfig.CodesFolder)) {
		t.Error("the folder was replaced: a unit running keeps hiding the old one")
	}
	if !os.SameFile(kept, r.stat(hubconfig.CodesFolder+"/access.json")) {
		t.Error("access.json in the folder was copied again")
	}
	for _, d := range codeDocs {
		r.linked(hubconfig.CodesFolder, d)
		r.linked(hubconfig.CodesFolder, d+".prev")
	}
	if slices.ContainsFunc(cmds, func(c string) bool { return strings.HasPrefix(c, "systemctl stop safehome-") }) {
		t.Errorf("a unit stopped with nothing to move: %q", cmds)
	}
}

// The hub from 29 October 2026 writes the codes in their folder alone: a flat file
// never linked is stale from its first write. Without the mark, the folder wins, kept
// as it is, and the flat file becomes the link; a flat previous version the folder has
// no counterpart of goes (stale too, and never a link to nothing).
func TestTheCodesFolderWinsBeforeTheFirstMove(t *testing.T) {
	r := newStateRig(t)
	doc := hubconfig.CodesFolder + "/access.json"
	writeFile(t, r.path(r.state+doc), "the folder's codes")
	writeFile(t, r.path(r.state+"access.json"), "stale flat codes")
	writeFile(t, r.path(r.state+"access.json.prev"), "staler flat codes")
	kept := r.stat(doc)

	r.mustRun()
	if got := r.read(doc); got != "the folder's codes" || !os.SameFile(kept, r.stat(doc)) {
		t.Errorf("%s = %q, want the folder's kept, not replaced", doc, got)
	}
	r.linked(hubconfig.CodesFolder, "access.json")
	for _, f := range []string{"access.json.prev", doc + ".prev"} {
		if _, err := os.Lstat(r.path(r.state + f)); !os.IsNotExist(err) {
			t.Errorf("%s: %v, want nothing", f, err)
		}
	}
}

// After the move, a rollback puts back a hub that writes the flat place: its filestore
// replaces the link with a file, and keeps as previous version a hard link to the link
// (Linux's link(2) does not follow it). With the mark, that flat document wins: it goes
// over the folder's, whose current version becomes the previous one.
func TestAFlatCodesFileWinsAfterARollback(t *testing.T) {
	r := newStateRig(t)
	writeFile(t, r.path(r.state+"access.json"), "codes before the rollback")
	r.mustRun()
	for _, f := range []string{"access.json.prev", "access.json"} {
		if err := os.Remove(r.path(r.state + f)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(hubconfig.CodesFolder+"/access.json", r.path(r.state+"access.json.prev")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, r.path(r.state+"access.json"), "codes written by the older hub")

	r.mustRun()
	doc := hubconfig.CodesFolder + "/access.json"
	if got := r.read(doc); got != "codes written by the older hub" {
		t.Errorf("%s = %q, want what the older hub wrote", doc, got)
	}
	if got := r.read(doc + ".prev"); got != "codes before the rollback" {
		t.Errorf("%s.prev = %q, want the folder's earlier version", doc, got)
	}
	r.linked(hubconfig.CodesFolder, "access.json")
	r.linked(hubconfig.CodesFolder, "access.json.prev")
}

// A move cut short (here a document it cannot read) leaves no mark: the next run is
// still a first move, where the folder's copies win over flat files never linked.
func TestACodesMoveCutShortLeavesNoMark(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without rights")
	}
	r := newStateRig(t)
	writeFile(t, r.path(r.state+"access.json"), "the codes")
	writeFile(t, r.path(r.state+"installer.json"), "the installer")
	if err := os.Chmod(r.path(r.state+"installer.json"), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.run(); err == nil {
		t.Fatal("install.sh went on past a copy that failed")
	}
	r.linked(hubconfig.CodesFolder, "access.json")
	if r.exists(r.state + codesMark(t)) {
		t.Errorf("%s made before every link is there", codesMark(t))
	}
	if fi := r.stat("installer.json"); !fi.Mode().IsRegular() {
		t.Errorf("installer.json: %v, want the flat file left", fi.Mode())
	}
	if err := os.Chmod(r.path(r.state+"installer.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.mustRun()
	if got := r.read(hubconfig.CodesFolder + "/installer.json"); got != "the installer" {
		t.Errorf("installer.json in the folder = %q", got)
	}
	r.linked(hubconfig.CodesFolder, "installer.json")
	if !r.exists(r.state + codesMark(t)) {
		t.Errorf("no %s once every link is there", codesMark(t))
	}
}
