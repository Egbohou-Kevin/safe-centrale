package qualitytest

import (
	"fmt"
	"go/ast"
	"strings"
	"testing"
)

// Size limits, in lines. A long function or file is hard to read and to review, and
// hides several responsibilities. Production code had none over these after the
// review; tests are allowed more because an end-to-end scenario reads best as one
// story (the longest, the agent's, is about 150 lines; the longest test file is about
// 700 lines).
const (
	maxFuncLines     = 50
	maxFileLines     = 400
	maxTestFuncLines = 160
	maxTestFileLines = 750
)

var sizeRule = rule{
	name: "size",
	fix:  "split the function into well-named steps, or the file by responsibility (same package)",
}

// oversized returns the functions and files longer than their limit. A function is
// counted from its func keyword to its closing brace, its doc comment excluded.
func oversized(tr *tree) []finding {
	var found []finding
	for _, f := range tr.files {
		maxFunc, maxFile, kind := maxFuncLines, maxFileLines, ""
		if f.test {
			maxFunc, maxFile, kind = maxTestFuncLines, maxTestFileLines, "test "
		}
		if n := tr.fset.File(f.ast.Pos()).LineCount(); n > maxFile {
			found = append(found, finding{
				pos: tr.fset.Position(f.ast.Pos()), key: f.rel,
				msg: fmt.Sprintf("%sfile has %d lines (limit %d)", kind, n, maxFile),
			})
		}
		for _, d := range f.ast.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			start, end := tr.pos(fn), tr.fset.Position(fn.End())
			if n := end.Line - start.Line + 1; n > maxFunc {
				found = append(found, finding{
					pos: start, key: f.rel + ":" + funcName(fn),
					msg: fmt.Sprintf("%sfunction %s has %d lines (limit %d)", kind, funcName(fn), n, maxFunc),
				})
			}
		}
	}
	return found
}

// funcName is "Name" for a function and "Type.Name" for a method.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return receiverType(fn.Recv.List[0].Type) + "." + fn.Name.Name
}

// receiverType is the name of a receiver's type, without pointer or type parameters.
func receiverType(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return receiverType(x.X)
	case *ast.IndexExpr:
		return receiverType(x.X)
	case *ast.IndexListExpr:
		return receiverType(x.X)
	case *ast.Ident:
		return x.Name
	}
	return "?"
}

func TestSizesStayWithinTheLimits(t *testing.T) {
	sizeRule.enforce(t, oversized(moduleTree(t)))
}

func TestSizeRuleCatchesLongFunctionsAndFiles(t *testing.T) {
	long := "package p\n\nfunc (r *R) Long() {\n" + strings.Repeat("\t_ = 1\n", maxFuncLines-1) + "}\n"
	longTest := "package p\n\nfunc TestLong() {\n" + strings.Repeat("\t_ = 1\n", maxFuncLines) + "}\n"
	bigFile := "package p\n" + strings.Repeat("\nvar _ = 1\n", maxFileLines/2)
	tr := sample(t, map[string]string{
		"p/long.go":      long,
		"p/ok_test.go":   longTest, // a test may be longer
		"p/big.go":       bigFile,
		"p/short.go":     "package p\n\nfunc Short() {}\n",
		"p/huge_test.go": "package p\n" + strings.Repeat("\n", maxTestFileLines),
	})
	got := strings.Join(keys(oversized(tr)), " ")
	for _, want := range []string{"p/long.go:R.Long", "p/big.go", "p/huge_test.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("the size rule missed %s (found %s)", want, got)
		}
	}
	if strings.Contains(got, "ok_test") || strings.Contains(got, "short") {
		t.Errorf("the size rule flagged code within the limits: %s", got)
	}
}
