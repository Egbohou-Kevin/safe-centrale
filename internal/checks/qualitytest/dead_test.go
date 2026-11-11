package qualitytest

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// An exported identifier of internal/ that nothing uses, or only its own package's
// tests, is dead code: it costs reading and keeping, and hides what the package is
// for. Exempt by rule: methods that satisfy an interface (they are called through
// it), the test-support packages, and the programs under cmd/ (package main). Uses
// in the package's own code count (unexport the name if you like, it is not dead);
// uses in other packages' tests count too.
var deadRule = rule{
	name: "dead",
	fix:  "delete it, or unexport it if only its own package needs it",
}

// typedPackage is one package of the module, type-checked from source.
type typedPackage struct {
	path  string // import path
	dir   string
	files []*file // non-test files
	tests []*file // test files of the same package
	xtest []*file // test files of the external package (name_test)
	types *types.Package
	info  *types.Info
}

// typedModule type-checks the module: every package with its own tests, so that uses
// in tests are seen. The standard library and third-party packages come from the
// compiler's export data (go list -export), which is fast and cached by the go tool.
type typedModule struct {
	tr       *tree
	pkgs     map[string]*typedPackage         // by import path
	external types.Importer                   // the packages outside the module
	uses     map[types.Object]map[string]bool // object -> files that use it
}

// newTypedModule prepares the check of the packages of tr; exports gives the export
// data file of each package outside the module (see exportData).
func newTypedModule(tr *tree, exports map[string]string) *typedModule {
	m := &typedModule{tr: tr, pkgs: map[string]*typedPackage{}, uses: map[types.Object]map[string]bool{}}
	m.external = importer.ForCompiler(tr.fset, "gc", func(p string) (io.ReadCloser, error) {
		f, ok := exports[p]
		if !ok || f == "" {
			return nil, fmt.Errorf("no export data for %s", p)
		}
		return os.Open(f)
	})
	for _, pf := range packagesOf(tr) {
		p := &typedPackage{path: module + "/" + pf.dir, dir: pf.dir}
		if pf.dir == "." {
			p.path = module
		}
		for _, f := range pf.files {
			switch {
			case !buildsHere(f):
				continue
			case !f.test:
				p.files = append(p.files, f)
			case strings.HasSuffix(f.ast.Name.Name, "_test"):
				p.xtest = append(p.xtest, f)
			default:
				p.tests = append(p.tests, f)
			}
		}
		m.pkgs[p.path] = p
	}
	return m
}

// buildsHere tells whether the go tool would build the file on this machine (build
// constraints, _linux.go suffixes). The few files for other systems are not
// type-checked: their uses are not seen.
func buildsHere(f *file) bool {
	ctxt := build.Default
	ctxt.OpenFile = func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(f.src)), nil }
	ok, err := ctxt.MatchFile(path.Dir(f.rel), path.Base(f.rel))
	return err == nil && ok
}

// Import gives a package of the module checked without its tests, checking it on
// first use, or any other package from its export data.
func (m *typedModule) Import(p string) (*types.Package, error) {
	pkg, ok := m.pkgs[p]
	if !ok || len(pkg.files) == 0 {
		return m.external.Import(p)
	}
	if pkg.types == nil {
		var err error
		pkg.types, pkg.info, err = m.check(p, pkg.files)
		if err != nil {
			return nil, err
		}
		m.record(pkg.info, "")
	}
	return pkg.types, nil
}

func (m *typedModule) check(p string, files []*file) (*types.Package, *types.Info, error) {
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
	var first error
	conf := types.Config{Importer: m, Error: func(err error) {
		if first == nil {
			first = err
		}
	}}
	asts := make([]*ast.File, len(files))
	for i, f := range files {
		asts[i] = f.ast
	}
	pkg, _ := conf.Check(p, m.tr.fset, asts, info)
	if first != nil {
		return nil, nil, fmt.Errorf("type-checking %s: %v", p, first)
	}
	return pkg, info, nil
}

// record notes, for each object of a module package, the files that use it. A check
// of a package with its tests sees copies of that package's objects: they are left
// out (skip), since those uses are the package's own.
func (m *typedModule) record(info *types.Info, skip string) {
	for id, obj := range info.Uses {
		if fn, ok := obj.(*types.Func); ok {
			obj = fn.Origin() // a method of an instance of a generic type
		}
		if obj.Pkg() == nil || obj.Pkg().Path() == skip {
			continue
		}
		if m.uses[obj] == nil {
			m.uses[obj] = map[string]bool{}
		}
		m.uses[obj][m.tr.fset.Position(id.Pos()).Filename] = true
	}
}

// load checks every package, then each one again with its in-package tests, then
// its external test package. The uses of a package's objects in its own tests are
// left out: they do not keep an identifier alive.
func (m *typedModule) load() error {
	paths := make([]string, 0, len(m.pkgs))
	for p := range m.pkgs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if len(m.pkgs[p].files) == 0 {
			continue // tests only: checked below
		}
		if _, err := m.Import(p); err != nil {
			return err
		}
	}
	for _, p := range paths {
		pkg := m.pkgs[p]
		variants := map[string][]*file{}
		if len(pkg.tests) > 0 {
			variants[p] = append(slices.Clone(pkg.files), pkg.tests...)
		}
		if len(pkg.xtest) > 0 {
			variants[p+"_test"] = pkg.xtest
		}
		for vp, files := range variants {
			_, info, err := m.check(vp, files)
			if err != nil {
				return err
			}
			m.record(info, p)
		}
	}
	return nil
}

// exportData asks the go tool for the export data of the packages outside the module
// that the module imports, tests included, and of their dependencies.
func exportData(tr *tree) (map[string]string, error) {
	imports := map[string]bool{}
	for _, f := range tr.files {
		for _, imp := range f.ast.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err == nil && p != module && !strings.HasPrefix(p, module+"/") && p != "C" {
				imports[p] = true
			}
		}
	}
	args := []string{"list", "-e", "-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}"}
	for p := range imports {
		args = append(args, p)
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = tr.root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -export: %v\n%s", err, stderr.String())
	}
	exports := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p, f, _ := strings.Cut(line, "\t")
		exports[p] = f
	}
	return exports, nil
}

// interfaceMethods returns, by name, the signatures of the methods of every interface
// the module can see: those it declares or writes (constraints included), those of
// the packages it imports, and error.
func (m *typedModule) interfaceMethods() map[string][]*types.Signature {
	out := map[string][]*types.Signature{}
	add := func(t types.Type) {
		iface, ok := t.Underlying().(*types.Interface)
		if !ok {
			return
		}
		for i := 0; i < iface.NumMethods(); i++ {
			fn := iface.Method(i)
			out[fn.Name()] = append(out[fn.Name()], fn.Type().(*types.Signature))
		}
	}
	add(types.Universe.Lookup("error").Type())
	seen := map[*types.Package]bool{}
	var walk func(p *types.Package)
	walk = func(p *types.Package) {
		if seen[p] {
			return
		}
		seen[p] = true
		for _, name := range p.Scope().Names() {
			if tn, ok := p.Scope().Lookup(name).(*types.TypeName); ok {
				add(tn.Type())
			}
		}
		for _, imp := range p.Imports() {
			walk(imp)
		}
	}
	for _, pkg := range m.pkgs {
		if pkg.types == nil {
			continue
		}
		walk(pkg.types)
		for _, tv := range pkg.info.Types {
			if tv.IsType() {
				add(tv.Type)
			}
		}
	}
	return out
}

// satisfiesInterface tells whether a method has the name and signature of a method of
// an interface, so that it may be called through that interface.
func satisfiesInterface(fn *types.Func, ifaces map[string][]*types.Signature) bool {
	for _, sig := range ifaces[fn.Name()] {
		if types.Identical(sig, fn.Type()) {
			return true
		}
	}
	return false
}

// dead returns the exported identifiers of internal/ that nothing but their own
// package's tests uses.
func (m *typedModule) dead() []finding {
	ifaces := m.interfaceMethods()
	var found []finding
	for _, pkg := range m.pkgs {
		if pkg.types == nil || !strings.HasPrefix(pkg.dir, "internal/") || isTestSupport(pkg.dir) {
			continue
		}
		scope := pkg.types.Scope()
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			if obj.Exported() && len(m.uses[obj]) == 0 {
				found = append(found, m.deadFinding(pkg, obj, name))
			}
			tn, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok {
				continue
			}
			for i := 0; i < named.NumMethods(); i++ {
				fn := named.Method(i)
				if fn.Exported() && len(m.uses[fn]) == 0 && !satisfiesInterface(fn, ifaces) {
					found = append(found, m.deadFinding(pkg, fn, name+"."+fn.Name()))
				}
			}
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].key < found[j].key })
	return found
}

func (m *typedModule) deadFinding(pkg *typedPackage, obj types.Object, name string) finding {
	kind := strings.ToLower(strings.TrimPrefix(fmt.Sprintf("%T", obj), "*types."))
	switch kind {
	case "typename":
		kind = "type"
	case "func":
		if obj.Type().(*types.Signature).Recv() != nil {
			kind = "method"
		}
	}
	return finding{
		pos: m.tr.fset.Position(obj.Pos()), key: pkg.dir + "." + name,
		msg: fmt.Sprintf("exported %s %s is used nowhere outside its own package's tests", kind, name),
	}
}

func TestNoDeadExportedCode(t *testing.T) {
	tr := moduleTree(t)
	exports, err := exportData(tr)
	if err != nil {
		t.Fatal(err)
	}
	m := newTypedModule(tr, exports)
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	deadRule.enforce(t, m.dead())
}

func TestDeadRuleCatchesUnusedExports(t *testing.T) {
	tr := sample(t, map[string]string{
		"internal/a/a.go": `package a
import "fmt"
func Used() int       { return 1 }
func Unused() int     { return 2 }
func OnlyTested() int { return 3 }
func Internal() int   { return Used() } // used by its own package: alive
var _ = Internal()
type T struct{}
func (T) String() string { return fmt.Sprint(1) } // satisfies fmt.Stringer
func (T) Dead()          {}
func (T) Called()        {}
const Limit = 3
`,
		"internal/a/a_test.go":      "package a\nvar _ = OnlyTested() + Limit\n",
		"internal/b/b.go":           "package b\nimport \"safehome/internal/a\"\nvar _ = a.Used()\nfunc f(t a.T) { t.Called() }\n",
		"internal/b/b_test.go":      "package b\nimport \"safehome/internal/a\"\nvar _ = a.Limit // another package's tests keep it alive\n",
		"internal/xtest/x.go":       "package xtest\nfunc Helper() {}\n",
		"cmd/p/main.go":             "package main\nfunc Exported() {}\nfunc main() {}\n",
		"internal/a/other_linux.go": "//go:build ignore\n\npackage a\nvar _ = Unused()\n",
	})
	exports, err := exportData(tr)
	if err != nil {
		t.Fatal(err)
	}
	m := newTypedModule(tr, exports)
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(keys(m.dead()), " ")
	if got != "internal/a.OnlyTested internal/a.T.Dead internal/a.Unused" {
		t.Errorf("dead rule: got %q, want OnlyTested, T.Dead and Unused", got)
	}
}
