// Package archtest holds architecture tests: they fail when the code breaks the
// dependency rules of the hexagonal design (see docs/architecture.md). It plays the
// role ArchUnit plays in Java projects.
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

// --- the rules ---------------------------------------------------------------

// The domain cores are pure: they know no wire format, no adapter and no I/O, and
// they never read the clock. This is what makes them testable with a simulated
// clock.
func TestDomainCoresArePure(t *testing.T) {
	ioPackages := []string{"os", "os/exec", "io/fs", "net", "net/http", "database/sql", "syscall", "log", "log/slog", "encoding/json"}
	for _, pkg := range []string{"engine", "health", "access", "escalation", "gsm", "zigbee", "video", "maint"} {
		fset, files := parsePackage(t, filepath.Join(root(t), "internal", pkg))
		for name, f := range files {
			bad := forbiddenImports(f, func(p string) bool {
				if isLocal(p) {
					return true // a domain core imports no other package of the module
				}
				if !isStdlib(p) {
					return true // and no third-party package
				}
				return slices.Contains(ioPackages, p)
			})
			if len(bad) > 0 {
				t.Errorf("internal/%s/%s imports %v: the domain must depend on nothing but the standard library, and not on I/O", pkg, name, bad)
			}
			for _, c := range clockCalls(fset, f) {
				t.Errorf("internal/%s/%s reads the clock (%s): inputs carry their own time", pkg, name, c)
			}
		}
	}
}

// The contract is the published language: it depends on nothing in the module.
func TestContractDependsOnNothing(t *testing.T) {
	_, files := parsePackage(t, filepath.Join(root(t), "internal", "contract"))
	for name, f := range files {
		if bad := forbiddenImports(f, func(p string) bool { return !isStdlib(p) }); len(bad) > 0 {
			t.Errorf("internal/contract/%s imports %v: the contract must stay standard-library only", name, bad)
		}
	}
}

// The driver runs a core in real time but knows no core: it is generic, and
// describes what it needs with functions. Adapters know the library or the disk they
// wrap and nothing of the domains, the contract or the driver. In both, the
// dependency arrows point one way only: toward the standard library (and, for an
// adapter, its own library).
func TestDriverAndAdaptersKnowNoDomain(t *testing.T) {
	dirs := []string{filepath.Join("internal", "driver")}
	// Every adapter, including those added later.
	entries, err := os.ReadDir(filepath.Join(root(t), "internal", "adapter"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join("internal", "adapter", e.Name()))
		}
	}
	if len(dirs) < 3 {
		t.Fatalf("found only %v: the adapters moved?", dirs)
	}
	for _, dir := range dirs {
		_, files := parsePackage(t, filepath.Join(root(t), dir))
		for name, f := range files {
			if bad := forbiddenImports(f, isLocal); len(bad) > 0 {
				t.Errorf("%s/%s imports %v: it must not know the domains, the contract or another package of the module", dir, name, bad)
			}
		}
	}
}

// The outbox serves every application of the site (hub, GSM gateway): it builds
// envelopes, so it knows the contract, and saves its counter through the driver's
// Store port. It knows no domain and no application.
func TestOutboxKnowsOnlyTheContractAndTheStorePort(t *testing.T) {
	allowed := []string{module + "/internal/contract", module + "/internal/driver"}
	_, files := parsePackage(t, filepath.Join(root(t), "internal", "outbox"))
	for name, f := range files {
		bad := forbiddenImports(f, func(p string) bool {
			if isLocal(p) {
				return !slices.Contains(allowed, p)
			}
			return !isStdlib(p)
		})
		if len(bad) > 0 {
			t.Errorf("internal/outbox/%s imports %v", name, bad)
		}
	}
}

// The agent's core moves contract messages: it knows the contract and nothing else,
// does no I/O and never reads the clock. Its application layer adds the driver and
// the outbox, and no adapter.
func TestTheAgentCoreKnowsOnlyTheContract(t *testing.T) {
	rules := map[string][]string{
		"agent":    {module + "/internal/contract"},
		"agentapp": {module + "/internal/agent", module + "/internal/contract", module + "/internal/driver", module + "/internal/outbox"},
	}
	for pkg, allowed := range rules {
		fset, files := parsePackage(t, filepath.Join(root(t), "internal", pkg))
		for name, f := range files {
			bad := forbiddenImports(f, func(p string) bool {
				if isLocal(p) {
					return !slices.Contains(allowed, p)
				}
				return !isStdlib(p)
			})
			if len(bad) > 0 {
				t.Errorf("internal/%s/%s imports %v", pkg, name, bad)
			}
			if pkg == "agent" {
				if calls := clockCalls(fset, f); len(calls) > 0 {
					t.Errorf("the agent's core reads the clock: %v", calls)
				}
			}
		}
	}
}

// The video service's application layer drives the streams through its own
// Streamer port: it knows the video core, its translation, the contract, the driver
// and the outbox, and no adapter (go2rtc is wired by cmd/safehome-video).
func TestVideoAppKnowsNoAdapter(t *testing.T) {
	allowed := []string{module + "/internal/video", module + "/internal/video/wire", module + "/internal/contract", module + "/internal/driver", module + "/internal/outbox"}
	_, files := parsePackage(t, filepath.Join(root(t), "internal", "videoapp"))
	for name, f := range files {
		bad := forbiddenImports(f, func(p string) bool {
			if isLocal(p) {
				return !slices.Contains(allowed, p)
			}
			return !isStdlib(p)
		})
		if len(bad) > 0 {
			t.Errorf("internal/videoapp/%s imports %v", name, bad)
		}
	}
}

// Remote maintenance's application layer opens the tunnel through its own Tunneler
// port: it knows the maintenance core, its translation, the contract, the driver and
// the outbox, and no adapter (ssh is wired by cmd/safehome-maint). It never reads the
// clock: the session's end is the core's.
func TestMaintAppKnowsNoAdapter(t *testing.T) {
	allowed := []string{module + "/internal/maint", module + "/internal/maint/wire", module + "/internal/contract", module + "/internal/driver", module + "/internal/outbox"}
	fset, files := parsePackage(t, filepath.Join(root(t), "internal", "maintapp"))
	for name, f := range files {
		bad := forbiddenImports(f, func(p string) bool {
			if isLocal(p) {
				return !slices.Contains(allowed, p)
			}
			return !isStdlib(p) || slices.Contains([]string{"os", "os/exec", "net", "syscall"}, p)
		})
		if len(bad) > 0 {
			t.Errorf("internal/maintapp/%s imports %v", name, bad)
		}
		if calls := clockCalls(fset, f); len(calls) > 0 {
			t.Errorf("internal/maintapp reads the clock: %v", calls)
		}
	}
}

// The configuration set from the cloud: the application that receives it knows no
// file and no process (checking, keeping and restarting are ports, wired by
// cmd/safehome-hub), and never reads the clock. The file that keeps it knows only the
// contract.
func TestTheCloudConfigurationKnowsNoAdapter(t *testing.T) {
	rules := map[string][]string{
		"configapp":  {module + "/internal/contract", module + "/internal/driver", module + "/internal/outbox"},
		"siteconfig": {module + "/internal/contract"},
	}
	for pkg, allowed := range rules {
		fset, files := parsePackage(t, filepath.Join(root(t), "internal", pkg))
		for name, f := range files {
			bad := forbiddenImports(f, func(p string) bool {
				if isLocal(p) {
					return !slices.Contains(allowed, p)
				}
				return !isStdlib(p) || (pkg == "configapp" && slices.Contains([]string{"os", "os/exec", "io/fs", "syscall"}, p))
			})
			if len(bad) > 0 {
				t.Errorf("internal/%s/%s imports %v", pkg, name, bad)
			}
			if pkg == "configapp" {
				if calls := clockCalls(fset, f); len(calls) > 0 {
					t.Errorf("internal/configapp reads the clock: %v", calls)
				}
			}
		}
	}
}

// The installer's application layer talks to the broker through its own ports and
// knows the site only as zones and sirens: no adapter, no configuration file, no hub.
// The command-line tool and the future web console both sit on it. It reads no
// clock either: the program gives it one.
func TestInstallKnowsOnlyTheContractAndTheZoneKinds(t *testing.T) {
	allowed := []string{module + "/internal/contract", module + "/internal/engine"}
	fset, files := parsePackage(t, filepath.Join(root(t), "internal", "install"))
	for name, f := range files {
		bad := forbiddenImports(f, func(p string) bool {
			if isLocal(p) {
				return !slices.Contains(allowed, p)
			}
			return !isStdlib(p)
		})
		if len(bad) > 0 {
			t.Errorf("internal/install/%s imports %v", name, bad)
		}
		if calls := clockCalls(fset, f); len(calls) > 0 {
			t.Errorf("internal/install reads the clock: %v", calls)
		}
	}
}

// The local console's application drives the network, the page and the hub's files
// through its own ports, and the devices through the installer's application: no
// adapter, no file, no process, no clock of its own. Its web page knows only it, and
// serves HTTP without touching the system.
func TestTheConsoleKnowsNoAdapter(t *testing.T) {
	rules := map[string]struct {
		allowed []string
		noStd   []string
	}{
		"consoleapp": {
			[]string{module + "/internal/contract", module + "/internal/driver", module + "/internal/install"},
			[]string{"os", "os/exec", "io/fs", "net", "net/http", "syscall"},
		},
		filepath.Join("consoleapp", "web"): {
			[]string{module + "/internal/consoleapp"},
			[]string{"os", "os/exec", "io/fs", "syscall"},
		},
	}
	for pkg, rule := range rules {
		fset, files := parsePackage(t, filepath.Join(root(t), "internal", pkg))
		for name, f := range files {
			bad := forbiddenImports(f, func(p string) bool {
				if isLocal(p) {
					return !slices.Contains(rule.allowed, p)
				}
				return !isStdlib(p) || slices.Contains(rule.noStd, p)
			})
			if len(bad) > 0 {
				t.Errorf("internal/%s/%s imports %v", pkg, name, bad)
			}
			if calls := clockCalls(fset, f); len(calls) > 0 {
				t.Errorf("internal/%s reads the clock: %v", pkg, calls)
			}
		}
	}
}

// The driver is where the real clock lives, but it takes it through the Clock
// interface: only SystemClock may read it.
func TestDriverReadsTheClockOnlyInSystemClock(t *testing.T) {
	fset, files := parsePackage(t, filepath.Join(root(t), "internal", "driver"))
	for name, f := range files {
		for _, c := range clockCalls(fset, f) {
			if !strings.Contains(c, "time.Now") && !strings.Contains(c, "time.NewTimer") {
				t.Errorf("internal/driver/%s uses the clock in an unexpected way: %s", name, c)
			}
		}
	}
	// Exactly two places: SystemClock.Now and SystemClock.Timer.
	var total int
	for _, f := range files {
		total += len(clockCalls(fset, f))
	}
	if total != 2 {
		t.Errorf("internal/driver reads the clock in %d places, want 2 (SystemClock.Now and SystemClock.Timer)", total)
	}
}

// The application layer (hub) assembles the domains through ports it declares
// itself. It never imports an adapter: wiring the real MQTT client or the real disk
// to those ports is the job of the composition root (cmd/), and nothing else.
func TestOnlyTheCompositionRootImportsAdapters(t *testing.T) {
	base := root(t)
	adapters := module + "/internal/adapter/"
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		if strings.HasPrefix(rel, "cmd"+string(filepath.Separator)) ||
			strings.HasPrefix(rel, filepath.Join("internal", "adapter")+string(filepath.Separator)) {
			return nil // the composition root and the adapters themselves
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range importsOf(f) {
			if strings.HasPrefix(imp, adapters) {
				t.Errorf("%s imports %s: only cmd/ may wire adapters", rel, imp)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The domains know nothing of the application layer, the driver or the adapters
// (this already follows from "a domain imports nothing of the module", and is
// stated here for the hub, which sits above them).
func TestNoDomainImportsTheHub(t *testing.T) {
	hub := module + "/internal/hub"
	for _, pkg := range []string{"engine", "health", "access", "escalation", "gsm", "zigbee", "video", "maint", "contract", "driver"} {
		_, files := parsePackage(t, filepath.Join(root(t), "internal", pkg))
		for name, f := range files {
			if slices.Contains(importsOf(f), hub) {
				t.Errorf("internal/%s/%s imports the hub", pkg, name)
			}
		}
	}
}

// Each wire package is the only bridge between one domain core and the contract.
// It does no I/O.
func TestWireOnlyBridgesItsDomainAndTheContract(t *testing.T) {
	for dir, domain := range map[string]string{
		filepath.Join("engine", "wire"):     "engine",
		filepath.Join("health", "wire"):     "health",
		filepath.Join("escalation", "wire"): "escalation",
		filepath.Join("gsm", "wire"):        "gsm",
		filepath.Join("zigbee", "wire"):     "zigbee",
		filepath.Join("video", "wire"):      "video",
		filepath.Join("maint", "wire"):      "maint",
	} {
		allowed := []string{module + "/internal/" + domain, module + "/internal/contract"}
		_, files := parsePackage(t, filepath.Join(root(t), "internal", dir))
		for name, f := range files {
			bad := forbiddenImports(f, func(p string) bool {
				if isLocal(p) {
					return !slices.Contains(allowed, p)
				}
				return !isStdlib(p) || slices.Contains([]string{"os", "net", "net/http", "database/sql"}, p)
			})
			if len(bad) > 0 {
				t.Errorf("internal/%s/%s imports %v", dir, name, bad)
			}
		}
	}
}

// The z2m package is the only one that knows Zigbee2MQTT's messages: it bridges them
// to the zigbee core, knows nothing of the contract, and does no I/O.
func TestZ2MOnlyBridgesZigbee2MQTTAndTheCore(t *testing.T) {
	allowed := []string{module + "/internal/zigbee"}
	_, files := parsePackage(t, filepath.Join(root(t), "internal", "zigbee", "z2m"))
	for name, f := range files {
		bad := forbiddenImports(f, func(p string) bool {
			if isLocal(p) {
				return !slices.Contains(allowed, p)
			}
			return !isStdlib(p) || slices.Contains([]string{"os", "net", "net/http", "database/sql", "log", "log/slog"}, p)
		})
		if len(bad) > 0 {
			t.Errorf("internal/zigbee/z2m/%s imports %v", name, bad)
		}
	}
}

// thirdPartyAllowed lists the only places where shipped code may import a
// third-party library: the adapter that wraps it. Everything else (domains,
// contract, drivers) stays on the standard library.
var thirdPartyAllowed = map[string][]string{
	// The MQTT library, and the WebSocket library it already uses: the adapter dials
	// WebSockets itself to send each MQTT packet in one message (see dial.go).
	filepath.Join("internal", "adapter", "mqtt"): {"github.com/eclipse/paho.golang", "github.com/gorilla/websocket"},
}

// allowedThirdParty reports whether a file at rel may import the package path.
func allowedThirdParty(rel, path string) bool {
	for dir, prefixes := range thirdPartyAllowed {
		if !strings.HasPrefix(rel, dir+string(filepath.Separator)) {
			continue
		}
		for _, prefix := range prefixes {
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				return true
			}
		}
	}
	return false
}

// Shipped code uses the standard library only, with one exception per adapter: the
// package that wraps a library may import it, and no other package may. Third-party
// packages are also allowed in tests, through internal/schematest.
func TestProductionCodeHasNoThirdPartyDependencies(t *testing.T) {
	base := root(t)
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		if strings.HasPrefix(rel, filepath.Join("internal", "schematest")) {
			return nil // test helper, imported by tests only (next rule)
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		if bad := forbiddenImports(f, func(p string) bool {
			return !isStdlib(p) && !isLocal(p) && !allowedThirdParty(rel, p)
		}); len(bad) > 0 {
			t.Errorf("%s imports third-party %v: only its adapter may (see thirdPartyAllowed)", rel, bad)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// internal/schematest pulls in a third-party validator, internal/adapter/mqtt/
// mqtttest starts a broker process and internal/adapter/atmodem/attest pretends to be
// a modem: shipped code must never import them.
func TestOnlyTestsImportTestSupport(t *testing.T) {
	for _, target := range []string{
		module + "/internal/schematest",
		module + "/internal/adapter/mqtt/mqtttest",
		module + "/internal/adapter/atmodem/attest",
		module + "/internal/adapter/go2rtc/go2rtctest",
	} {
		onlyTestsImport(t, target)
	}
}

func onlyTestsImport(t *testing.T, target string) {
	t.Helper()
	base := root(t)
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		if slices.Contains(importsOf(f), target) {
			rel, _ := filepath.Rel(base, path)
			t.Errorf("%s imports %s, which is for tests only", rel, target)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
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

func TestThirdPartyExceptionIsNarrow(t *testing.T) {
	mq := filepath.Join("internal", "adapter", "mqtt", "client.go")
	cases := []struct {
		rel, path string
		want      bool
	}{
		{mq, "github.com/eclipse/paho.golang/autopaho", true},
		{mq, "github.com/eclipse/paho.golang", true},
		{mq, "github.com/gorilla/websocket", true},
		{mq, "github.com/gorilla/mux", false},
		{mq, "github.com/eclipse/paho.golangx", false},
		{filepath.Join("internal", "engine", "engine.go"), "github.com/eclipse/paho.golang/autopaho", false},
		{filepath.Join("internal", "adapter", "mqttx", "a.go"), "github.com/eclipse/paho.golang/autopaho", false},
		{filepath.Join("internal", "adapter", "other", "a.go"), "github.com/eclipse/paho.golang/autopaho", false},
		{filepath.Join("internal", "agent", "agent.go"), "github.com/gorilla/websocket", false},
	}
	for _, c := range cases {
		if got := allowedThirdParty(c.rel, c.path); got != c.want {
			t.Errorf("allowedThirdParty(%s, %s) = %v, want %v", c.rel, c.path, got, c.want)
		}
	}
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
