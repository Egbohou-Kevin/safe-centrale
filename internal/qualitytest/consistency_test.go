package qualitytest

import (
	"bufio"
	"bytes"
	"go/ast"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Consistency rules fixed during the review, that must not come back.

// The error of a log line is always under the key "err": one key to search the
// journal for, on every program.
var slogRule = rule{name: "slog", fix: `log the error under the key "err"`}

// Waiting on the real clock makes code slow and tests flaky: application code waits
// on the driver's Clock port (timers that a fake clock fires). Only adapters, which
// face real processes and devices, and test-support packages may sleep.
var sleepRule = rule{
	name: "sleep",
	fix:  "wait on the driver.Clock port (a timer), or on a channel; in tests, drivertest.WaitFor or the fake clock",
}

// A sum type is sealed by a marker method named after it: Action has isAction, Event
// has isEvent. One name per role makes the domains read alike.
var markerRule = rule{
	name: "marker",
	fix:  "name the marker method is<Interface> (isAction for Action) and declare it on the interface",
}

// Every package says what it is for, in a doc comment that starts with "Package
// <name>" ("Command <program>" for a program), as go doc shows it.
var packageDocRule = rule{
	name: "packagedoc",
	fix:  `add a doc comment "// Package name ..." (or "// Command name ...") above the package clause of one file`,
}

// The README's tables are the map of the code: every package of internal/ and cmd/
// has its row, and no row names a package that is gone.
var readmeRule = rule{name: "readme", fix: "add, rename or remove the row in the package tables of README.md"}

// importName is the name under which a file imports the package at path, or "".
func importName(f *ast.File, importPath string) string {
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err != nil || p != importPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return path.Base(importPath)
	}
	return ""
}

// calledFunc returns X and Sel of a call written X.Sel(...), if it is one.
func calledFunc(call *ast.CallExpr) (string, string) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	if id, ok := sel.X.(*ast.Ident); ok {
		return id.Name, sel.Sel.Name
	}
	return "", sel.Sel.Name
}

// slogLead is, for each logging method of slog, how many arguments come before the
// key-value pairs (the message, the context, the level).
var slogLead = map[string]int{
	"Debug": 1, "Info": 1, "Warn": 1, "Error": 1,
	"DebugContext": 2, "InfoContext": 2, "WarnContext": 2, "ErrorContext": 2,
	"Log": 3, "With": 0,
}

// slogErrorKeys returns the log calls of files that import log/slog which put a
// value under the key "error".
func slogErrorKeys(tr *tree) []finding {
	var found []finding
	for _, f := range tr.files {
		slog := importName(f.ast, "log/slog")
		if slog == "" {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			x, name := calledFunc(call)
			var keyArgs []ast.Expr
			if lead, ok := slogLead[name]; ok && len(call.Args) > lead {
				keyArgs = pairKeys(call.Args[lead:])
			} else if x == slog && len(call.Args) > 0 { // slog.Any("error", err), slog.String...
				keyArgs = call.Args[:1]
			}
			for _, k := range keyArgs {
				if lit, ok := k.(*ast.BasicLit); ok && lit.Kind == token.STRING && lit.Value == `"error"` {
					found = append(found, finding{pos: tr.pos(lit), key: f.rel, msg: `log key "error"`})
				}
			}
			return true
		})
	}
	return found
}

// pairKeys returns the keys of slog's arguments: a string is a key followed by its
// value; anything else is an Attr.
func pairKeys(args []ast.Expr) []ast.Expr {
	var keys []ast.Expr
	for i := 0; i < len(args); i++ {
		if lit, ok := args[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			keys = append(keys, lit)
			i++
		}
	}
	return keys
}

// sleeps returns the calls to time.Sleep in the shipped code of internal/, adapters
// and test-support packages aside.
func sleeps(tr *tree) []finding {
	var found []finding
	for _, f := range tr.files {
		if f.test || !strings.HasPrefix(f.rel, "internal/") || strings.HasPrefix(f.rel, "internal/adapter/") || isTestSupport(f.dir()) {
			continue
		}
		timePkg := importName(f.ast, "time")
		ast.Inspect(f.ast, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if x, name := calledFunc(call); timePkg != "" && x == timePkg && name == "Sleep" {
					found = append(found, finding{pos: tr.pos(call), key: f.rel, msg: "time.Sleep in application code"})
				}
			}
			return true
		})
	}
	return found
}

func TestErrorsAreLoggedUnderErr(t *testing.T) { slogRule.enforce(t, slogErrorKeys(moduleTree(t))) }

func TestApplicationCodeDoesNotSleep(t *testing.T) { sleepRule.enforce(t, sleeps(moduleTree(t))) }

func TestLogAndSleepRulesCatchViolations(t *testing.T) {
	tr := sample(t, map[string]string{
		"internal/app/a.go": `package app
import ("log/slog"; "time")
func f(log *slog.Logger, err error) {
	log.Error("cannot", "site", "x", "error", err)
	log.Info("fine", "err", err, "status", "error") // "error" is a value here
	slog.Warn("x", slog.Any("error", err))
	time.Sleep(time.Second)
}
`,
		"internal/adapter/dev/d.go":        "package dev\nimport \"time\"\nfunc f() { time.Sleep(1) }\n",
		"internal/driver/drivertest/w.go":  "package drivertest\nimport \"time\"\nfunc f() { time.Sleep(1) }\n",
		"internal/app/a_test.go":           "package app\nimport \"time\"\nfunc g() { time.Sleep(1) }\n",
		"internal/other/no_slog.go":        "package other\nfunc f(t T) { t.Error(\"x\", \"error\") }\n",
		"internal/other/clock/realtime.go": "package clock\nimport t \"time\"\nfunc f() { t.Sleep(1) }\n",
	})
	if got := slogErrorKeys(tr); len(got) != 2 || got[0].pos.Line != 4 || got[1].pos.Line != 6 {
		t.Errorf("slog rule: got %+v, want lines 4 and 6 of internal/app/a.go", got)
	}
	got := keys(sleeps(tr))
	slices.Sort(got)
	if !slices.Equal(got, []string{"internal/app/a.go", "internal/other/clock/realtime.go"}) {
		t.Errorf("sleep rule: got %v", got)
	}
}

// markerViolations returns the marker interfaces whose method is not named after
// them, and the marker methods (unexported, no argument, no result, empty body) that
// seal no interface of their package.
func markerViolations(tr *tree) []finding {
	var found []finding
	for _, pkg := range packagesOf(tr) {
		markers := map[string]bool{}
		for _, f := range pkg.files {
			found = append(found, markerInterfaces(tr, f, markers)...)
		}
		for _, f := range pkg.files {
			for _, d := range f.ast.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if ok && isMarkerMethod(fn) && !markers[fn.Name.Name] {
					found = append(found, finding{pos: tr.pos(fn), key: f.rel + ":" + funcName(fn),
						msg: "marker method " + fn.Name.Name + " seals no interface of the package"})
				}
			}
		}
	}
	return found
}

// markerInterfaces checks the marker interfaces of a file and adds their method to
// markers.
func markerInterfaces(tr *tree, f *file, markers map[string]bool) []finding {
	var found []finding
	ast.Inspect(f.ast, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		it, ok := ts.Type.(*ast.InterfaceType)
		if !ok || it.Methods == nil || len(it.Methods.List) != 1 {
			return false
		}
		m := it.Methods.List[0]
		ft, ok := m.Type.(*ast.FuncType)
		if !ok || len(m.Names) != 1 || m.Names[0].IsExported() || ft.Params.NumFields() > 0 || ft.Results.NumFields() > 0 {
			return false
		}
		markers[m.Names[0].Name] = true
		if want := "is" + ts.Name.Name; m.Names[0].Name != want {
			found = append(found, finding{pos: tr.pos(m), key: f.rel + ":" + ts.Name.Name,
				msg: "the marker method of " + ts.Name.Name + " is " + m.Names[0].Name + ", want " + want})
		}
		return false
	})
	return found
}

func isMarkerMethod(fn *ast.FuncDecl) bool {
	return fn.Recv != nil && !fn.Name.IsExported() && fn.Type.Params.NumFields() == 0 &&
		fn.Type.Results.NumFields() == 0 && fn.Body != nil && len(fn.Body.List) == 0
}

func TestMarkerMethodsAreNamedAfterTheirInterface(t *testing.T) {
	markerRule.enforce(t, markerViolations(moduleTree(t)))
}

func TestMarkerRuleCatchesViolations(t *testing.T) {
	tr := sample(t, map[string]string{
		"p/a.go": `package p
type Action interface{ isAction() }
type Event interface{ event() }
type Input interface{ at() int }
func (Go) isAction() {}
func (Bang) event()   {}
func (Go) isActon()  {}
`,
	})
	got := strings.Join(keys(markerViolations(tr)), " ")
	if got != "p/a.go:Event p/a.go:Go.isActon" {
		t.Errorf("marker rule: got %q, want the Event interface and the misspelt isActon", got)
	}
}

// missingPackageDocs returns the packages without a doc comment of the right form.
// For a package of tests only, the doc may sit in a test file.
func missingPackageDocs(tr *tree) []finding {
	var found []finding
	for _, pkg := range packagesOf(tr) {
		var candidates []*file
		for _, f := range pkg.files {
			if !f.test {
				candidates = append(candidates, f)
			}
		}
		if len(candidates) == 0 {
			candidates = pkg.files
		}
		if !slices.ContainsFunc(candidates, hasPackageDoc) {
			found = append(found, finding{pos: token.Position{Filename: pkg.dir}, key: pkg.dir, msg: "the package has no doc comment"})
		}
	}
	return found
}

func hasPackageDoc(f *file) bool {
	if f.ast.Doc == nil {
		return false
	}
	want := "Package " + f.ast.Name.Name + " "
	if f.ast.Name.Name == "main" {
		want = "Command " + path.Base(f.dir()) + " "
	}
	return strings.HasPrefix(f.ast.Doc.Text(), want)
}

func TestEveryPackageHasADocComment(t *testing.T) {
	packageDocRule.enforce(t, missingPackageDocs(moduleTree(t)))
}

func TestPackageDocRuleCatchesViolations(t *testing.T) {
	tr := sample(t, map[string]string{
		"a/a.go":           "// Package a does a.\npackage a\n",
		"b/b.go":           "package b\n",
		"b/b_test.go":      "// Package b is tested.\npackage b\n", // the doc must be on shipped code
		"c/c.go":           "// C does c.\npackage c\n",
		"t/t_test.go":      "// Package t holds tests.\npackage t\n",
		"cmd/prog/main.go": "// Command prog runs.\npackage main\n",
	})
	got := strings.Join(keys(missingPackageDocs(tr)), " ")
	if got != "b c" {
		t.Errorf("package doc rule: got %q, want \"b c\"", got)
	}
}

// readmeRows returns the paths named in the first cell of the README's table rows.
func readmeRows(readme []byte) map[string]int {
	rows := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(readme))
	for line := 1; sc.Scan(); line++ {
		cells := strings.Split(sc.Text(), "|")
		if len(cells) < 3 || strings.TrimSpace(cells[0]) != "" {
			continue
		}
		first := strings.TrimSpace(cells[1])
		if p, ok := strings.CutPrefix(first, "`"); ok && strings.HasSuffix(p, "`") {
			rows[strings.TrimSuffix(strings.TrimSuffix(p, "`"), "/")] = line
		}
	}
	return rows
}

// readmeMismatches returns the packages of internal/ and cmd/ without a row, and the
// rows that name a package that does not exist.
func readmeMismatches(tr *tree, readme []byte) []finding {
	rows := readmeRows(readme)
	var found []finding
	pkgs := map[string]bool{}
	for _, pkg := range packagesOf(tr) {
		if !strings.HasPrefix(pkg.dir, "internal/") && !strings.HasPrefix(pkg.dir, "cmd/") {
			continue
		}
		pkgs[pkg.dir] = true
		if _, ok := rows[pkg.dir]; !ok {
			found = append(found, finding{pos: token.Position{Filename: "README.md"}, key: pkg.dir, msg: pkg.dir + " has no row in the package tables"})
		}
	}
	for p, line := range rows {
		if (strings.HasPrefix(p, "internal/") || strings.HasPrefix(p, "cmd/")) && !pkgs[p] {
			found = append(found, finding{pos: token.Position{Filename: "README.md", Line: line}, key: p, msg: "the row names " + p + ", which is not a package"})
		}
	}
	slices.SortFunc(found, func(a, b finding) int { return strings.Compare(a.key, b.key) })
	return found
}

func TestTheReadmeListsEveryPackage(t *testing.T) {
	tr := moduleTree(t)
	readme, err := os.ReadFile(filepath.Join(tr.root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readmeRule.enforce(t, readmeMismatches(tr, readme))
}

func TestReadmeRuleCatchesViolations(t *testing.T) {
	tr := sample(t, map[string]string{
		"internal/a/a.go": "package a\n",
		"internal/b/b.go": "package b\n",
		"cmd/p/main.go":   "package main\n",
	})
	readme := []byte("# x\n\n| Dossier | Contenu |\n|---|---|\n| `internal/a` | A |\n| `cmd/p` | P |\n| `internal/gone` | G |\n| `schemas/` | S |\n")
	got := keys(readmeMismatches(tr, readme))
	slices.Sort(got)
	if !slices.Equal(got, []string{"internal/b", "internal/gone"}) {
		t.Errorf("readme rule: got %v, want [internal/b internal/gone]", got)
	}
}
