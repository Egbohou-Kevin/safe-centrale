package qualitytest

import (
	"fmt"
	"go/scanner"
	"go/token"
	"hash/fnv"
	"sort"
	"strings"
	"testing"
)

// Copy-paste is caught on tokens, so layout and comments do not hide it, and so are
// renamed variables: an identifier is compared by where it last appeared in the
// stretch, not by its name ("a := f(a)" matches "b := f(b)", not "a := f(b)").
// Names after a dot (fields, methods, package members), names of imported packages
// and literals are compared as written, so code of the same shape that calls other
// things (the same wiring for two domains) is not a copy. Imports are skipped, and
// so are stretches that are mostly one short pattern repeated (lists of one-line
// methods). The thresholds are about 12 to 15 lines of production code and 20 of
// test code (tests repeat their setup on purpose, not whole helpers). The longest
// copies left after the review: 117 tokens in production (the same daemon wiring in
// two programs), 122 in tests.
const (
	maxCloneTokens     = 130
	maxTestCloneTokens = 200
)

var (
	duplicationRule = rule{
		name: "duplication",
		fix:  "factor the shared code into one function (in the same package, or in the package both already depend on)",
	}
	testDuplicationRule = rule{
		name: "test-duplication",
		fix:  "factor the shared code into one helper (in the test files of the package, or in a test-support package)",
	}
)

// dupToken is one token of a file. An identifier that is not after a dot has prev,
// the index of its last appearance in the file (-1 if none); any other token has a
// fixed code.
type dupToken struct {
	code uint64
	prev int
	pos  token.Pos
}

const identCode = 1 << 62 // marks a renameable identifier; the code then adds a distance

// tokenize returns the tokens of a file, imports and package clause left out.
func tokenize(fset *token.FileSet, name string, src []byte) []dupToken {
	var s scanner.Scanner
	s.Init(fset.AddFile(name, -1, len(src)), src, nil, 0)
	var toks []dupToken
	last := map[string]int{}
	imported := map[string]bool{}
	afterDot, header, depth, alias := false, false, 0, ""
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return toks
		}
		if tok == token.PACKAGE || tok == token.IMPORT {
			header = true
		}
		if header {
			header, depth, alias = importToken(tok, lit, depth, alias, imported)
			continue
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue // layout, not code
		}
		t := dupToken{code: tokenCode(tok, lit, afterDot || imported[lit]), prev: -1, pos: pos}
		if tok == token.IDENT && !afterDot && !imported[lit] {
			t.code = identCode
			if i, ok := last[lit]; ok {
				t.prev = i
			}
			last[lit] = len(toks)
		}
		afterDot = tok == token.PERIOD
		toks = append(toks, t)
	}
}

// importToken follows a package clause or an import declaration, given the depth of
// parentheses so far and the alias just read, and notes the names of the imported
// packages (they are compared as written: another package is not a renamed variable).
// It reports whether the header goes on.
func importToken(tok token.Token, lit string, depth int, alias string, imported map[string]bool) (bool, int, string) {
	switch tok {
	case token.LPAREN:
		depth++
	case token.RPAREN:
		depth--
	case token.IDENT, token.PERIOD:
		return true, depth, lit
	case token.STRING:
		name := alias
		if name == "" {
			name = packageName(lit)
		}
		imported[name] = true
		return true, depth, ""
	case token.SEMICOLON:
		return depth > 0, depth, ""
	}
	return true, depth, alias
}

// packageName guesses the name of a package from its quoted import path: its last
// element, or the one before a major version ("jsonschema/v6").
func packageName(quoted string) string {
	p := strings.Trim(quoted, "`\"")
	elems := strings.Split(p, "/")
	name := elems[len(elems)-1]
	if len(elems) > 1 && len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		name = elems[len(elems)-2]
	}
	return name
}

// tokenCode is the fixed code of a token: its kind, and its text for literals and
// names after a dot.
func tokenCode(tok token.Token, lit string, afterDot bool) uint64 {
	if !tok.IsLiteral() || (tok == token.IDENT && !afterDot) {
		return uint64(tok)
	}
	h := fnv.New64a()
	fmt.Fprintf(h, "%d:%s", tok, lit)
	return h.Sum64() &^ identCode
}

// windowHash hashes the n tokens of toks from start, each renameable identifier coded
// by the distance to its last appearance inside the window.
func windowHash(toks []dupToken, start, n int) uint64 {
	h := uint64(14695981039346656037)
	for i := start; i < start+n; i++ {
		c := toks[i].code
		if c == identCode && toks[i].prev >= start {
			c += uint64(i - toks[i].prev)
		}
		h = (h ^ c) * 1099511628211
	}
	return h
}

// spot is a window: a file and the index of its first token.
type spot struct{ file, at int }

// clone is a stretch of tokens found twice: in a, and again in b, length tokens long.
type clone struct {
	a, b   spot
	length int
}

// clones returns the stretches of at least n tokens that appear twice, in one file or
// in two. Overlapping copies of a repeating pattern inside one file are not clones.
func clones(files [][]dupToken, n int) []clone {
	seen := map[uint64][]spot{}
	open := map[[2]spot]*clone{} // by the last window of each clone being extended
	var found []*clone
	for fi, toks := range files {
		for at := 0; at+n <= len(toks); at++ {
			h := windowHash(toks, at, n)
			here := spot{fi, at}
			for _, there := range seen[h] {
				if there.file == fi && at-there.at < n {
					continue
				}
				key := [2]spot{{there.file, there.at - 1}, {fi, at - 1}}
				c, ok := open[key]
				if ok {
					delete(open, key)
					c.length++
				} else {
					c = &clone{a: there, b: here, length: n}
					found = append(found, c)
				}
				open[[2]spot{there, here}] = c
			}
			seen[h] = append(seen[h], here)
		}
	}
	out := make([]clone, len(found))
	for i, c := range found {
		out[i] = *c
	}
	return out
}

// duplicates returns the copies of more than limit tokens among the files that
// keep says to look at; each copy is reported once, against its first appearance.
func duplicates(tr *tree, limit int, keep func(*file) bool) []finding {
	fset := token.NewFileSet()
	var picked []*file
	var toks [][]dupToken
	for _, f := range tr.files {
		if keep(f) {
			picked = append(picked, f)
			toks = append(toks, tokenize(fset, f.rel, f.src))
		}
	}
	found := clones(toks, limit+1)
	sort.SliceStable(found, func(i, j int) bool { return found[i].length > found[j].length })
	reported := map[spot]bool{}
	var out []finding
	for _, c := range found {
		if reported[c.b] || repetitive(toks[c.b.file][c.b.at:c.b.at+c.length]) {
			continue
		}
		reported[c.b] = true
		a, b := picked[c.a.file], picked[c.b.file]
		first := fset.Position(toks[c.a.file][c.a.at].pos)
		last := fset.Position(toks[c.a.file][c.a.at+c.length-1].pos)
		out = append(out, finding{
			pos: fset.Position(toks[c.b.file][c.b.at].pos), key: b.rel + "~" + a.rel,
			msg: fmt.Sprintf("%d tokens copied from %s:%d-%d (limit %d)", c.length, a.rel, first.Line, last.Line, limit),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pos.String() < out[j].pos.String() })
	return out
}

// repetitive tells whether a stretch is mostly one short pattern repeated, such as a
// list of one-line methods (at, isAction) next to a declaration: a copy of it is not
// worth a function. Three quarters of its tokens must repeat the pattern.
func repetitive(toks []dupToken) bool {
	for p := 1; p <= len(toks)/3; p++ {
		same := 0
		for i := p; i < len(toks); i++ {
			if toks[i].code == toks[i-p].code {
				same++
			}
		}
		if 4*same >= 3*(len(toks)-p) {
			return true
		}
	}
	return false
}

func TestNoCopiedProductionCode(t *testing.T) {
	duplicationRule.enforce(t, duplicates(moduleTree(t), maxCloneTokens, func(f *file) bool { return !f.test }))
}

func TestNoCopiedTestCode(t *testing.T) {
	testDuplicationRule.enforce(t, duplicates(moduleTree(t), maxTestCloneTokens, func(f *file) bool { return f.test }))
}

// dupSample is a function of about 90 tokens; the names in it can be replaced.
const dupSample = `package p

import (
	"fmt"
	"strings"
)

func process(items []string, limit int) (int, error) {
	count := 0
	for _, item := range items {
		if len(item) > limit {
			return count, fmt.Errorf("too long: %q", item)
		}
		switch {
		case strings.HasPrefix(item, "#"):
			continue
		case item == "":
			count--
		default:
			count += len(item) * 2
		}
	}
	if count < 0 {
		count = 0
	}
	return count, nil
}
`

func TestDuplicationRuleCatchesCopies(t *testing.T) {
	renamed := strings.NewReplacer("process", "tally", "items", "words", "limit", "max", "count", "n", "item", "w").Replace(dupSample)
	otherCalls := strings.NewReplacer("HasPrefix", "HasSuffix", "Errorf", "Sprintf").Replace(dupSample)
	var markers strings.Builder
	markers.WriteString("package q\n\ntype Action interface{ isAction() }\n\n")
	for _, name := range []string{"Send", "Ring", "Stop", "Start", "Report", "Drop", "Keep", "Load", "Save", "Wait"} {
		markers.WriteString("func (" + name + ") isAction() {}\n")
	}
	tr := sample(t, map[string]string{
		"p/a.go": dupSample, "p/b.go": renamed, "p/c.go": otherCalls,
		"q/a.go": markers.String(), "q/b.go": strings.Replace(markers.String(), "package q", "package q // again", 1),
	})
	got := keys(duplicates(tr, 50, func(*file) bool { return true }))
	if len(got) != 1 || !strings.HasPrefix(got[0], "p/b.go~p/a.go") && !strings.HasPrefix(got[0], "p/a.go~p/b.go") {
		t.Errorf("duplication rule: got %v, want only the renamed copy (p/a.go and p/b.go)", got)
	}
}
