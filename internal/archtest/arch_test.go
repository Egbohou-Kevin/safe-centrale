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
	return filepath.Join(filepath.Dir(file), "..", "..")
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
		out[i] = module + "/internal/" + n
	}
	return out
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
	"safehome/internal/contract"
	"github.com/someone/lib"
)
`)
	pure := func(p string) bool { return isLocal(p) || !isStdlib(p) || p == "os" }
	got := forbiddenImports(f, pure)
	want := []string{"os", "safehome/internal/contract", "github.com/someone/lib"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if isStdlib("safehome/internal/contract") || !isLocal("safehome/internal/contract") || isLocal("safehomex/y") {
		t.Fatal("module path classification is wrong")
	}
}

func TestALayerRejectsWhatItMustReject(t *testing.T) {
	l := layer{local: internal("contract"), noStd: []string{"os"}}
	for p, want := range map[string]bool{
		"fmt":                        false,
		"os":                         true,
		"safehome/internal/contract": false,
		"safehome/internal/engine":   true,
		"github.com/someone/lib":     true,
	} {
		if got := l.rejects(p); got != want {
			t.Errorf("rejects(%s) = %v, want %v", p, got, want)
		}
	}
}
