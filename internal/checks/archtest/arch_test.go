// Package archtest holds architecture tests: they fail when the code breaks the
// dependency rules of the hexagonal design (see docs/architecture.md). It plays the
// role ArchUnit plays in Java projects.
//
// This file holds the tools and their own tests; layers_test.go the rules of each
// layer, deps_test.go the rules on third-party libraries and test support.
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const module = "safehome"

func root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the source tree")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func isLocal(path string) bool { return path == module || strings.HasPrefix(path, module+"/") }

func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".") && !isLocal(path)
}

// internal returns the import paths of the module's packages under internal/.
func internal(names ...string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = module + "/internal/" + at(n)
	}
	return out
}

// moved is the index of the rules: they name a package by its short name, its path
// in the flat layout internal/ had until 11/11/2026, and at turns it into its place
// in the layout by program (docs/rangement-par-programme.md, programs_test.go).
var moved = map[string]string{
	"hub":               "hub/app/hub",
	"access":            "shared/core/access",
	"site":              "shared/core/site",
	"zones":             "shared/core/zones",
	"sitecodes":         "shared/sitecodes",
	"adapter/machine":   "hub/adapter/machine",
	"configapp":         "hub/app/configapp",
	"engine":            "hub/core/engine",
	"escalation":        "hub/core/escalation",
	"health":            "hub/core/health",
	"hubconfig":         "hub/config/hubconfig",
	"adapter/hotspot":   "console/adapter/hotspot",
	"consoleapp":        "console/app/consoleapp",
	"consoleconfig":     "console/config/consoleconfig",
	"install":           "shared/install",
	"zigbee":            "zigbee/core/zigbee",
	"zigbeeapp":         "zigbee/app/zigbeeapp",
	"zigbeeconfig":      "zigbee/config/zigbeeconfig",
	"gsm":               "gsm/core/gsm",
	"adapter/atmodem":   "gsm/adapter/atmodem",
	"gateway":           "gsm/app/gateway",
	"gsmconfig":         "gsm/config/gsmconfig",
	"agent":             "agent/core/agent",
	"sealcode":          "agent/core/sealcode",
	"adapter/queuedir":  "agent/adapter/queuedir",
	"adapter/upload":    "agent/adapter/upload",
	"agentapp":          "agent/app/agentapp",
	"agentconfig":       "agent/config/agentconfig",
	"video":             "video/core/video",
	"adapter/go2rtc":    "video/adapter/go2rtc",
	"videoapp":          "video/app/videoapp",
	"videoconfig":       "video/config/videoconfig",
	"maint":             "maint/core/maint",
	"adapter/sshtunnel": "maint/adapter/sshtunnel",
	"maintapp":          "maint/app/maintapp",
	"maintconfig":       "maint/config/maintconfig",
	"power":             "power/core/power",
	"adapter/x728":      "power/adapter/x728",
	"powerapp":          "power/app/powerapp",
	"powerconfig":       "power/config/powerconfig",
	"archtest":          "checks/archtest",
	"qualitytest":       "checks/qualitytest",
	"contract":          "shared/contract",
	"schematest":        "shared/contract/schematest",
	"driver":            "shared/driver",
	"inbox":             "shared/inbox",
	"outbox":            "shared/outbox",
	"service":           "shared/service",
	"configfile":        "shared/config/configfile",
	"siteconfig":        "shared/config/siteconfig",
	"adapter/filelock":  "shared/adapter/filelock",
	"adapter/filestore": "shared/adapter/filestore",
	"adapter/mqtt":      "shared/adapter/mqtt",
	"adapter/photodir":  "shared/adapter/photodir",
	"adapter/pidfile":   "shared/adapter/pidfile",
	"adapter/sdnotify":  "shared/adapter/sdnotify",
	"adapter/sysclock":  "shared/adapter/sysclock",
}

// at is the current path below internal/ of the package once at internal/old.
func at(old string) string {
	best := ""
	for k := range moved {
		if (old == k || strings.HasPrefix(old, k+"/")) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return old
	}
	return moved[best] + old[len(best):]
}

// dirOf is the directory, relative to the root, of the package once at internal/old.
func dirOf(old string) string { return filepath.Join("internal", filepath.FromSlash(at(old))) }

// inAdapter reports whether p, a file path relative to the root or an import path,
// lies in an adapter: a package whose parent directory below internal/ is named adapter.
func inAdapter(p string) bool {
	p = strings.TrimPrefix(filepath.ToSlash(p), module+"/")
	segs := strings.Split(p, "/")
	if len(segs) < 3 || segs[0] != "internal" {
		return false
	}
	return slices.Contains(segs[1:len(segs)-1], "adapter")
}

// packageDirs lists the directories below internal/ that hold shipped Go files,
// slash-separated and relative to internal/.
func packageDirs(t *testing.T) []string {
	t.Helper()
	base := filepath.Join(root(t), "internal")
	var dirs []string
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, _ := filepath.Rel(base, filepath.Dir(path))
		if rel = filepath.ToSlash(rel); !slices.Contains(dirs, rel) {
			dirs = append(dirs, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

// parsePackage parses the non-test Go files of one directory (not recursive).
func parsePackage(t *testing.T, dir string) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatalf("no Go file in %s: the rule would check nothing", dir)
	}
	return fset, files
}

// walkSources parses the imports of every non-test Go file of the module and gives
// each to check, with its path relative to the module's root.
func walkSources(t *testing.T, check func(rel string, f *ast.File)) {
	t.Helper()
	base := root(t)
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// a session's worktree under .claude/ is a copy of the module, not its sources
		if d.IsDir() && path != base && d.Name() == ".claude" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, path)
		check(rel, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func importsOf(f *ast.File) []string {
	var out []string
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err == nil {
			out = append(out, p)
		}
	}
	return out
}

// forbiddenImports returns the imports of f that rule rejects.
func forbiddenImports(f *ast.File, rejects func(path string) bool) []string {
	var bad []string
	for _, p := range importsOf(f) {
		if rejects(p) {
			bad = append(bad, p)
		}
	}
	return bad
}

// clockCalls returns the calls to time functions that read or wait on the clock.
func clockCalls(fset *token.FileSet, f *ast.File) []string {
	clock := []string{"Now", "Since", "Until", "Sleep", "After", "AfterFunc", "NewTimer", "NewTicker", "Tick"}
	var found []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" && slices.Contains(clock, sel.Sel.Name) {
			found = append(found, fset.Position(sel.Pos()).String()+": time."+sel.Sel.Name)
		}
		return true
	})
	return found
}

// layer says what the shipped files of a package may use: no third-party package,
// the module's packages in local only, the standard library but noStd.
type layer struct {
	local   []string // import paths, see internal()
	noStd   []string
	noClock bool   // it must not read the clock either
	why     string // said with each violation
}

// rejects reports whether the layer forbids the import path p.
func (l layer) rejects(p string) bool {
	if isLocal(p) {
		return !slices.Contains(l.local, p)
	}
	return !isStdlib(p) || slices.Contains(l.noStd, p)
}

// check reports every import and clock call of the package internal/dir that the
// layer forbids.
func (l layer) check(t *testing.T, dir string) {
	t.Helper()
	dir = at(filepath.ToSlash(dir))
	fset, files := parsePackage(t, filepath.Join(root(t), "internal", dir))
	for name, f := range files {
		if bad := forbiddenImports(f, l.rejects); len(bad) > 0 {
			t.Errorf("internal/%s/%s imports %v%s", dir, name, bad, l.why)
		}
		if !l.noClock {
			continue
		}
		for _, c := range clockCalls(fset, f) {
			t.Errorf("internal/%s/%s reads the clock (%s)%s", dir, name, c, l.why)
		}
	}
}

// --- the rules must be able to fail ---------------------------------------------

func parseSource(t *testing.T, src string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	return fset, f
}

func TestClockDetectorCatchesViolations(t *testing.T) {
	fset, f := parseSource(t, `package p
import "time"
func a() time.Time { return time.Now() }
func b(t time.Time) time.Duration { return time.Since(t) }
func c() { time.Sleep(time.Second) }
func ok(t time.Time) time.Time { return t.Add(time.Minute) } // arithmetic is fine
`)
	got := clockCalls(fset, f)
	if len(got) != 3 {
		t.Fatalf("want 3 clock calls (Now, Since, Sleep), got %v", got)
	}
}

func TestImportDetectorCatchesViolations(t *testing.T) {
	_, f := parseSource(t, `package p
import (
	"fmt"
	"os"
	"safehome/internal/shared/contract"
	"github.com/someone/lib"
)
`)
	pure := func(p string) bool { return isLocal(p) || !isStdlib(p) || p == "os" }
	got := forbiddenImports(f, pure)
	want := []string{"os", "safehome/internal/shared/contract", "github.com/someone/lib"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if isStdlib("safehome/internal/shared/contract") || !isLocal("safehome/internal/shared/contract") || isLocal("safehomex/y") {
		t.Fatal("module path classification is wrong")
	}
}

func TestALayerRejectsWhatItMustReject(t *testing.T) {
	l := layer{local: internal("contract"), noStd: []string{"os"}}
	for p, want := range map[string]bool{
		"fmt":                               false,
		"os":                                true,
		"safehome/internal/shared/contract": false,
		"safehome/internal/hub/core/engine": true,
		"github.com/someone/lib":            true,
	} {
		if got := l.rejects(p); got != want {
			t.Errorf("rejects(%s) = %v, want %v", p, got, want)
		}
	}
}
