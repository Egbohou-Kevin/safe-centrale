package qualitytest

import (
	"bytes"
	"fmt"
	"go/format"
	"go/token"
	"strings"
	"testing"
	"unicode/utf8"
)

// Every Go file is gofmt-formatted: diffs then show only real changes, and nobody
// argues about layout.
var gofmtRule = rule{name: "gofmt", fix: "run gofmt -w on the file"}

// Go files are plain ASCII, code, comments and tests alike (the project's rule): no
// accents, no typographic quotes or dashes. French text lives in templates and docs.
var asciiRule = rule{
	name: "ascii",
	fix:  "write it in plain ASCII English (straight quotes, '-' for dashes); French text belongs in templates or docs",
}

// unformatted returns the files that gofmt would change.
func unformatted(tr *tree) []finding {
	var found []finding
	for _, f := range tr.files {
		out, err := format.Source(f.src)
		if err != nil || !bytes.Equal(out, f.src) {
			found = append(found, finding{pos: token.Position{Filename: f.rel}, key: f.rel, msg: "the file is not gofmt-formatted"})
		}
	}
	return found
}

// nonASCII returns the first non-ASCII character of each line that has one.
func nonASCII(tr *tree) []finding {
	var found []finding
	for _, f := range tr.files {
		for i, line := range strings.Split(string(f.src), "\n") {
			for col, r := range line {
				if r < utf8.RuneSelf {
					continue
				}
				found = append(found, finding{
					pos: token.Position{Filename: f.rel, Line: i + 1, Column: col + 1},
					key: f.rel, msg: fmt.Sprintf("non-ASCII character %U", r),
				})
				break
			}
		}
	}
	return found
}

func TestEveryFileIsFormatted(t *testing.T) {
	gofmtRule.enforce(t, unformatted(moduleTree(t)))
}

func TestEveryFileIsASCII(t *testing.T) {
	asciiRule.enforce(t, nonASCII(moduleTree(t)))
}

func TestTextRulesCatchBadFiles(t *testing.T) {
	tr := sample(t, map[string]string{
		"p/messy.go":  "package p\nfunc  F( ) {}\n",
		"p/french.go": "package p\n\n// F renvoie l'" + string(rune(0xE9)) + "tat.\nfunc F() {}\n",
		"p/clean.go":  "package p\n\n// F returns the state.\nfunc F() {}\n",
	})
	if got := keys(unformatted(tr)); len(got) != 1 || got[0] != "p/messy.go" {
		t.Errorf("gofmt rule: got %v, want [p/messy.go]", got)
	}
	got := nonASCII(tr)
	if len(got) != 1 || got[0].key != "p/french.go" || got[0].pos.Line != 3 || !strings.Contains(got[0].msg, "U+00E9") {
		t.Errorf("ascii rule: got %+v, want p/french.go line 3, U+00E9", got)
	}
}
