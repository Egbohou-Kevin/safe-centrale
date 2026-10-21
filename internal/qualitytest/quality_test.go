// Package qualitytest holds the code-quality guardrails: size, formatting, plain
// ASCII text, duplication, dead code, re-declared test fakes and a few consistency
// rules fixed during the review. They run with go test ./... so that new code cannot
// drift back. The dependency rules live in internal/archtest.
//
// Each failure reads "file:line: [rule] what is wrong. Fix: what to do". A justified
// exception is never a skip: it goes in allowed (exceptions_test.go) with its reason,
// and an entry that no longer matches anything fails too, so the list cannot rot.
//
// This file holds the shared tools: loading the module's sources once, and reporting.
package qualitytest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

const module = "safehome"

// file is one Go source file of the module.
type file struct {
	rel  string // slash-separated path from the module's root: "internal/hub/hub.go"
	test bool   // a _test.go file
	src  []byte
	ast  *ast.File
}

// dir is the slash-separated directory of the file, from the module's root.
func (f *file) dir() string { return path.Dir(f.rel) }

// tree is every Go file of the module, parsed with its comments. The file names in
// fset are the rel paths, so positions print as "internal/hub/hub.go:12:3".
type tree struct {
	root  string
	fset  *token.FileSet
	files []*file
}

var (
	loadOnce sync.Once
	loaded   *tree
	loadErr  error
)

// moduleTree parses the module once for all the rules of this package.
func moduleTree(t *testing.T) *tree {
	t.Helper()
	loadOnce.Do(func() { loaded, loadErr = loadTree(moduleRoot(t)) })
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	return loaded
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the source tree")
	}
	return filepath.Join(filepath.Dir(here), "..", "..")
}

// skipDir tells the directories that hold no source of the module.
func skipDir(name string) bool {
	return name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func loadTree(root string) (*tree, error) {
	tr := &tree{root: root, fset: token.NewFileSet()}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		f, err := parseFile(tr.fset, filepath.ToSlash(rel), src)
		if err != nil {
			return err
		}
		tr.files = append(tr.files, f)
		return nil
	})
	return tr, err
}

func parseFile(fset *token.FileSet, rel string, src []byte) (*file, error) {
	a, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	return &file{rel: rel, test: strings.HasSuffix(rel, "_test.go"), src: src, ast: a}, nil
}

// sample parses in-memory sources for the rules' own tests: name -> source.
func sample(t *testing.T, sources map[string]string) *tree {
	t.Helper()
	tr := &tree{fset: token.NewFileSet()}
	for _, name := range slices.Sorted(maps.Keys(sources)) { // in order, as on disk
		f, err := parseFile(tr.fset, name, []byte(sources[name]))
		if err != nil {
			t.Fatal(err)
		}
		tr.files = append(tr.files, f)
	}
	return tr
}

// isTestSupport tells a package that only tests may import (drivertest, outboxtest,
// mqtttest, schematest...) or that holds only tests (archtest, qualitytest): by
// convention, its name ends in "test".
func isTestSupport(dir string) bool { return strings.HasSuffix(path.Base(dir), "test") }

// finding is one breach of a rule.
type finding struct {
	pos token.Position // where; Line 0 for a whole file
	key string         // what an exception names: a file, or "file:Name"
	msg string
}

// rule is a guardrail: its name (as exceptions name it) and what to do on a breach.
type rule struct {
	name string
	fix  string
}

// enforce reports every finding not covered by an exception, and every exception of
// the rule that matched nothing.
func (r rule) enforce(t *testing.T, found []finding) {
	t.Helper()
	used := map[string]bool{}
	for _, f := range found {
		if allows(r.name, f.key) {
			used[f.key] = true
			continue
		}
		where := f.pos.Filename
		if f.pos.Line > 0 {
			where = f.pos.String()
		}
		t.Errorf("%s: [%s] %s. Fix: %s", where, r.name, f.msg, r.fix)
	}
	for _, e := range allowed {
		if e.rule == r.name && !used[e.key] {
			t.Errorf("exceptions_test.go: [%s] the exception for %s matches nothing any more. Fix: remove it", r.name, e.key)
		}
	}
}

// allows tells whether an exception covers the finding.
func allows(rule, key string) bool {
	for _, e := range allowed {
		if e.rule == rule && e.key == key {
			return true
		}
	}
	return false
}

// keys returns the keys of the findings, to check the rules' own tests.
func keys(found []finding) []string {
	out := make([]string, len(found))
	for i, f := range found {
		out[i] = f.key
	}
	return out
}

// pos is the position of a node of a file of tr.
func (tr *tree) pos(n ast.Node) token.Position { return tr.fset.Position(n.Pos()) }
