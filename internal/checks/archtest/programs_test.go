package archtest

import (
	"go/ast"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The layout of internal/ by program (docs/rangement-par-programme.md, 11/11/2026):
// each program has its folder, holding its application, cores, own adapters and
// configuration; what several programs use lies in shared/, the guardrails in checks/.

// programs are the folders of internal/ that each belong to one program of cmd/.
var programs = []string{"agent", "console", "gsm", "hub", "install", "maint", "power", "video", "zigbee"}

// roles are the folders a program's packages sit in.
var roles = []string{"adapter", "app", "config", "core"}

// crossPrograms are the only packages of another program a program may import,
// with the reason. Decided alone, to be reviewed (docs/rangement-par-programme.md,
// case 1).
var crossPrograms = map[string]map[string]string{
	"console": {
		"hub/app/hub":          "the local console shows the hub's state the way the hub computes it",
		"hub/config/hubconfig": "the console reads and edits the hub's configuration",
		"install/app/install":  "the console's pairing pages sit on the installer",
	},
	"install": {
		"hub/config/hubconfig": "the installer writes the hub's site configuration",
		"hub/core/engine":      "the installer checks zone and sensor kinds with the engine's rule",
	},
}

// roleOf says why the package at dir (slash-separated, relative to internal/) has
// its place, or "" when it has none.
func roleOf(dir string) string {
	segs := strings.Split(dir, "/")
	switch {
	case segs[0] == "shared" && len(segs) > 1:
		return "shared"
	case segs[0] == "checks" && len(segs) == 2:
		return "checks"
	case slices.Contains(programs, segs[0]) && len(segs) >= 3 && slices.Contains(roles, segs[1]):
		return segs[1]
	}
	return ""
}

// programOf is the program a source file belongs to (its path relative to the root,
// slash-separated), or "".
func programOf(rel string) string {
	segs := strings.Split(rel, "/")
	switch {
	case len(segs) > 2 && segs[0] == "internal" && slices.Contains(programs, segs[1]):
		return segs[1]
	case len(segs) > 2 && segs[0] == "cmd" && slices.Contains(programs, strings.TrimPrefix(segs[1], "safehome-")):
		return strings.TrimPrefix(segs[1], "safehome-")
	}
	return ""
}

// crossProgram reports whether a file of program from may not import imp: a package
// of another program that crossPrograms does not name.
func crossProgram(from, imp string) bool {
	rest, ok := strings.CutPrefix(imp, module+"/internal/")
	if !ok || from == "" {
		return false
	}
	other, _, _ := strings.Cut(rest, "/")
	if other == from || !slices.Contains(programs, other) {
		return false
	}
	_, allowed := crossPrograms[from][rest]
	return !allowed
}

// roleOfImport says which role an import path of the module lies in.
func roleOfImport(imp string) string {
	rest, ok := strings.CutPrefix(imp, module+"/internal/")
	if !ok {
		return ""
	}
	if inAdapter(imp) {
		return "adapter"
	}
	if r := roleOf(rest); slices.Contains(roles, r) {
		return r
	}
	if strings.HasPrefix(rest, "shared/service") {
		return "adapter"
	}
	return ""
}

// coreRejects reports whether a core may not import imp: an adapter (its own or a
// shared one, the service assembly included) or an application.
func coreRejects(imp string) bool {
	r := roleOfImport(imp)
	return r == "adapter" || r == "app"
}

func TestEveryPackageHasARole(t *testing.T) {
	dirs := packageDirs(t)
	if len(dirs) < 60 {
		t.Fatalf("found only %d packages: the walk is broken", len(dirs))
	}
	for _, d := range dirs {
		if roleOf(d) == "" {
			t.Errorf("internal/%s has no place: a package lies in <program>/{%s}/, shared/ or checks/", d, strings.Join(roles, ","))
		}
	}
}

func TestAProgramImportsNoOtherProgram(t *testing.T) {
	seen := map[string]bool{}
	walkSources(t, func(rel string, f *ast.File) {
		from := programOf(filepath.ToSlash(rel))
		seen[from] = true
		for _, imp := range importsOf(f) {
			if crossProgram(from, imp) {
				t.Errorf("%s (program %s) imports %s: a program imports only its own packages and shared/", rel, from, imp)
			}
		}
	})
	for _, p := range programs {
		if !seen[p] {
			t.Errorf("no source file found for program %s: the rule would check nothing", p)
		}
	}
}

func TestACoreImportsNoAdapterNorApplication(t *testing.T) {
	n := 0
	walkSources(t, func(rel string, f *ast.File) {
		segs := strings.Split(filepath.ToSlash(rel), "/")
		if len(segs) < 4 || segs[0] != "internal" || segs[2] != "core" {
			return
		}
		n++
		for _, imp := range importsOf(f) {
			if coreRejects(imp) {
				t.Errorf("%s imports %s: a core knows neither an adapter nor an application", rel, imp)
			}
		}
	})
	if n == 0 {
		t.Fatal("no core found: the rule would check nothing")
	}
}

// --- these rules must be able to fail -------------------------------------------

func TestTheLayoutRulesRejectWhatTheyMustReject(t *testing.T) {
	for d, want := range map[string]string{
		"hub/core/engine":        "core",
		"hub/core/engine/wire":   "core",
		"gsm/adapter/atmodem":    "adapter",
		"shared/adapter/mqtt":    "shared",
		"checks/archtest":        "checks",
		"engine":                 "",
		"hub/engine":             "",
		"shared":                 "",
		"nowhere/core/x":         "",
		"checks/archtest/deeper": "",
	} {
		if got := roleOf(d); got != want {
			t.Errorf("roleOf(%s) = %q, want %q", d, got, want)
		}
	}
	for _, c := range []struct {
		from, imp string
		bad       bool
	}{
		{"video", module + "/internal/hub/core/engine", true},
		{"video", module + "/internal/video/core/video", false},
		{"video", module + "/internal/shared/contract", false},
		{"console", module + "/internal/hub/config/hubconfig", false},
		{"console", module + "/internal/shared/core/access", false},
		{"hub", module + "/internal/shared/core/site", false},
		{"hub", module + "/internal/install/app/install", true},
		{"console", module + "/internal/hub/core/engine/wire", true},
		{"console", module + "/internal/hub/core/engine", true},
		{"", module + "/internal/hub/core/engine", false},
		{"gsm", "github.com/someone/lib", false},
	} {
		if got := crossProgram(c.from, c.imp); got != c.bad {
			t.Errorf("crossProgram(%s, %s) = %v, want %v", c.from, c.imp, got, c.bad)
		}
	}
	if programOf("cmd/safehome-gsm/run.go") != "gsm" || programOf("internal/shared/inbox/x.go") != "" || programOf("deploy/x.go") != "" {
		t.Error("programOf misplaces a file")
	}
	for imp, bad := range map[string]bool{
		module + "/internal/hub/adapter/machine": true,
		module + "/internal/shared/adapter/mqtt": true,
		module + "/internal/shared/service":      true,
		module + "/internal/video/app/videoapp":  true,
		module + "/internal/hub/core/health":     false,
		module + "/internal/shared/contract":     false,
		"os":                                     false,
	} {
		if got := coreRejects(imp); got != bad {
			t.Errorf("coreRejects(%s) = %v, want %v", imp, got, bad)
		}
	}
}
