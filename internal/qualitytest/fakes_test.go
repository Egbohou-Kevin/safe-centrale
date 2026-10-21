package qualitytest

import (
	"go/ast"
	"slices"
	"strings"
	"testing"
)

// The review merged the copies of the test fakes and of a few helpers into one place
// each. A new copy must not appear: tests use the shared one, and may wrap it
// (embed it) to add checks of their own.
var fakesRule = rule{
	name: "fakes",
	fix:  "use the shared one named in the message, or embed it in your type to add behaviour",
}

// sharedHelpers maps the lower-case names of the merged copies to what replaces them.
var sharedHelpers = map[string]string{
	"fakeclock":       "drivertest.FakeClock",
	"faketimer":       "drivertest.FakeClock (its Timer)",
	"memstore":        "drivertest.MemStore",
	"syncbuffer":      "drivertest.SyncBuffer (drivertest.Logs)",
	"waitfor":         "drivertest.WaitFor",
	"eventually":      "drivertest.WaitFor",
	"fakebroker":      "outboxtest.Broker",
	"decodeorder":     "inbox.Open",
	"statusrepeater":  "driver.Every",
	"brokerpublisher": "service.Publisher",
}

// sharedHomes are the packages that hold the shared fakes, and may declare them.
var sharedHomes = []string{"internal/driver/drivertest", "internal/outbox/outboxtest"}

// redeclaredFakes returns the types and functions, outside the shared homes, named
// like a merged copy, and the test types that play the clock or the store (they have
// its methods), unless they embed a shared fake. A test type that publishes is not
// flagged by its methods: several tests check what is published on the way, and the
// outbox's own tests cannot import outboxtest (it imports the outbox).
func redeclaredFakes(tr *tree) []finding {
	var found []finding
	for _, pkg := range packagesOf(tr) {
		if slices.Contains(sharedHomes, pkg.dir) {
			continue
		}
		methods := methodNames(pkg.files)
		for _, f := range pkg.files {
			for _, d := range f.ast.Decls {
				found = append(found, fakeDecl(tr, f, d, methods)...)
			}
		}
	}
	return found
}

// fakeDecl checks one declaration of a file.
func fakeDecl(tr *tree, f *file, d ast.Decl, methods map[string][]string) []finding {
	if fn, ok := d.(*ast.FuncDecl); ok {
		if use, ok := sharedHelpers[strings.ToLower(fn.Name.Name)]; ok && fn.Recv == nil {
			return []finding{{pos: tr.pos(fn), key: f.rel + ":" + fn.Name.Name, msg: "function " + fn.Name.Name + " re-declares " + use}}
		}
		return nil
	}
	gd, ok := d.(*ast.GenDecl)
	if !ok {
		return nil
	}
	var found []finding
	for _, s := range gd.Specs {
		ts, ok := s.(*ast.TypeSpec)
		if !ok || embedsSharedFake(ts) {
			continue
		}
		use, named := sharedHelpers[strings.ToLower(ts.Name.Name)]
		if !named && f.test {
			use = playedFake(methods[ts.Name.Name])
		}
		if use != "" {
			found = append(found, finding{pos: tr.pos(ts), key: f.rel + ":" + ts.Name.Name, msg: "type " + ts.Name.Name + " re-declares " + use})
		}
	}
	return found
}

// playedFake tells which shared fake a type with these methods plays, if any.
func playedFake(methods []string) string {
	has := func(names ...string) bool {
		for _, n := range names {
			if !slices.Contains(methods, n) {
				return false
			}
		}
		return true
	}
	switch {
	case has("Now", "Timer"):
		return "drivertest.FakeClock"
	case has("Save", "Load"):
		return "drivertest.MemStore"
	}
	return ""
}

// embedsSharedFake tells whether a struct type embeds a type of a shared home.
func embedsSharedFake(ts *ast.TypeSpec) bool {
	st, ok := ts.Type.(*ast.StructType)
	if !ok {
		return false
	}
	for _, field := range st.Fields.List {
		if len(field.Names) > 0 {
			continue
		}
		typ := field.Type
		if star, ok := typ.(*ast.StarExpr); ok {
			typ = star.X
		}
		if sel, ok := typ.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "drivertest" || id.Name == "outboxtest") {
				return true
			}
		}
	}
	return false
}

// methodNames maps each type of a package to the names of its methods.
func methodNames(files []*file) map[string][]string {
	out := map[string][]string{}
	for _, f := range files {
		for _, d := range f.ast.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv != nil && len(fn.Recv.List) > 0 {
				typ := receiverType(fn.Recv.List[0].Type)
				out[typ] = append(out[typ], fn.Name.Name)
			}
		}
	}
	return out
}

// pkgFiles is the files of one directory, tests included.
type pkgFiles struct {
	dir   string
	files []*file
}

// packagesOf groups the files of tr by directory, in a stable order.
func packagesOf(tr *tree) []pkgFiles {
	var out []pkgFiles
	index := map[string]int{}
	for _, f := range tr.files {
		i, ok := index[f.dir()]
		if !ok {
			i = len(out)
			index[f.dir()] = i
			out = append(out, pkgFiles{dir: f.dir()})
		}
		out[i].files = append(out[i].files, f)
	}
	return out
}

func TestNoRedeclaredFakes(t *testing.T) {
	fakesRule.enforce(t, redeclaredFakes(moduleTree(t)))
}

func TestFakesRuleCatchesCopies(t *testing.T) {
	tr := sample(t, map[string]string{
		"p/a_test.go": `package p
type fakeClock struct{}
type clock struct{}
func (clock) Now() time.Time { return time.Time{} }
func (clock) Timer(d time.Duration) (<-chan time.Time, func()) { return nil, nil }
type store struct{}
func (*store) Save(string, []byte) error { return nil }
func (*store) Load(string) ([]byte, bool, error) { return nil, false, nil }
type wrapped struct{ *drivertest.MemStore }
func (wrapped) Save(string, []byte) error { return nil }
func (wrapped) Load(string) ([]byte, bool, error) { return nil, false, nil }
func eventually(cond func() bool) {}
`,
		"p/prod.go": `package p
type brokerPublisher struct{}
type adapter struct{}
func (adapter) Publish() error { return nil } // production code may publish
`,
		"internal/driver/drivertest/d.go": "package drivertest\ntype memStore struct{}\n",
	})
	got := strings.Join(keys(redeclaredFakes(tr)), " ")
	want := "p/a_test.go:fakeClock p/a_test.go:clock p/a_test.go:store p/a_test.go:eventually p/prod.go:brokerPublisher"
	for _, w := range strings.Fields(want) {
		if !strings.Contains(got+" ", w+" ") {
			t.Errorf("the fakes rule missed %s (found %s)", w, got)
		}
	}
	for _, bad := range []string{"wrapped", "adapter", "memStore"} {
		if strings.Contains(got, bad) {
			t.Errorf("the fakes rule flagged %s, which is allowed (found %s)", bad, got)
		}
	}
}
