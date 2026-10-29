package archtest

import (
	"cmp"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

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
	walkSources(t, func(rel string, f *ast.File) {
		if strings.HasPrefix(rel, filepath.Join("internal", "schematest")) {
			return // test helper, imported by tests only (next rule)
		}
		if bad := forbiddenImports(f, func(p string) bool {
			return !isStdlib(p) && !isLocal(p) && !allowedThirdParty(rel, p)
		}); len(bad) > 0 {
			t.Errorf("%s imports third-party %v: only its adapter may (see thirdPartyAllowed)", rel, bad)
		}
	})
}

// internal/schematest pulls in a third-party validator, internal/adapter/mqtt/
// mqtttest starts a broker process, internal/adapter/atmodem/attest pretends to be
// a modem, internal/adapter/go2rtc/go2rtctest a video relay,
// internal/driver/drivertest fakes the clock and the disk and
// internal/outbox/outboxtest the broker: shipped code must never import them. The
// virtual kit's fake devices (vkit/) are test equipment too, and may.
func TestOnlyTestsImportTestSupport(t *testing.T) {
	testSupport := []string{
		module + "/internal/schematest",
		module + "/internal/driver/drivertest",
		module + "/internal/outbox/outboxtest",
		module + "/internal/adapter/mqtt/mqtttest",
		module + "/internal/adapter/atmodem/attest",
		module + "/internal/adapter/go2rtc/go2rtctest",
	}
	walkSources(t, func(rel string, f *ast.File) {
		if inVirtualKit(rel) {
			return
		}
		for _, imp := range importsOf(f) {
			if slices.Contains(testSupport, imp) {
				t.Errorf("%s imports %s, which is for tests only", rel, imp)
			}
		}
	})
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

// allowedExec reports whether a file at rel may run other programs (os/exec): the
// adapters, which wrap a process (ssh, go2rtc, nmcli, mosquitto in tests), and the
// programs of cmd/, which call the system's tools (mmcli, systemctl). A domain or
// an application layer that runs a process would hide a side effect from its tests.
func allowedExec(rel string) bool {
	return strings.HasPrefix(rel, filepath.Join("internal", "adapter")+string(filepath.Separator)) ||
		strings.HasPrefix(rel, "cmd"+string(filepath.Separator))
}

// Only adapters and programs run other programs: os/exec nowhere else in shipped code.
func TestOnlyAdaptersAndProgramsRunProcesses(t *testing.T) {
	walkSources(t, func(rel string, f *ast.File) {
		if allowedExec(rel) {
			return
		}
		if bad := forbiddenImports(f, func(p string) bool { return p == "os/exec" }); len(bad) > 0 {
			t.Errorf("%s imports os/exec: only an adapter (internal/adapter/) or a program (cmd/) may run a process", rel)
		}
	})
}

func TestExecRuleIsNarrow(t *testing.T) {
	cases := []struct {
		rel  string
		want bool
	}{
		{filepath.Join("internal", "adapter", "sshtunnel", "sshtunnel.go"), true},
		{filepath.Join("cmd", "safehome-gsm", "modem.go"), true},
		{filepath.Join("internal", "hub", "hub.go"), false},
		{filepath.Join("internal", "adapterx", "a.go"), false},
		{filepath.Join("internal", "cmd", "a.go"), false},
		{filepath.Join("vkit", "a.go"), false},
	}
	for _, c := range cases {
		if got := allowedExec(c.rel); got != c.want {
			t.Errorf("allowedExec(%s) = %v, want %v", c.rel, got, c.want)
		}
	}
}

// openBrokers are the tests that may start an open broker (mqtttest.StartOpen or
// StartOpenWith), with their number of calls: the MQTT client's own tests (the whole of
// internal/adapter/mqtt) and the agent's cloud side. A program under test runs on the
// hub's broker, with the shipped ACL (29 October 2026).
var openBrokers = map[string]int{
	filepath.Join("cmd", "safehome-agent", "run_test.go"): 1,
}

// brokerNames gives the name a program has on the hub's broker when it is not the name
// of its folder: safehome-install connects as the operator (configfile.AsOperator).
var brokerNames = map[string]string{"safehome-install": "operator"}

// testBrokers counts the open brokers a test file starts and returns the names it
// starts the hub's broker for (mqtttest.Start(t, "name"), a literal name only).
func testBrokers(f *ast.File) (open int, names []string) {
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		switch {
		case !ok:
		case isSelector(c.Fun, "mqtttest", "StartOpen"), isSelector(c.Fun, "mqtttest", "StartOpenWith"):
			open++
		case isSelector(c.Fun, "mqtttest", "Start") && len(c.Args) == 2:
			if lit, ok := c.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if name, err := strconv.Unquote(lit.Value); err == nil {
					names = append(names, name)
				}
			}
		}
		return true
	})
	return open, names
}

// Each program's tests run it on the hub's broker with the shipped ACL, under its own
// name (mqtttest.Start), so that a right forgotten in deploy/mosquitto/safehome.acl
// leaves it deaf and a test fails. An open broker serves only the MQTT client's tests
// and the agent's cloud side (openBrokers).
func TestProgramsAreTestedOnTheHubsBrokerWithItsRights(t *testing.T) {
	base := root(t)
	started := map[string]bool{} // "dir/name": dir's tests start the hub's broker for name
	open := map[string]int{}
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != base && d.Name() == ".claude" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, path)
		n, names := testBrokers(f)
		if n > 0 {
			open[rel] = n
		}
		for _, name := range names {
			started[filepath.Dir(rel)+"/"+name] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel, n := range open {
		if !strings.HasPrefix(rel, filepath.Join("internal", "adapter", "mqtt")+string(filepath.Separator)) && openBrokers[rel] != n {
			t.Errorf("%s starts %d open broker(s), %d allowed: test a program on mqtttest.Start, under its name", rel, n, openBrokers[rel])
		}
	}
	dirs, err := filepath.Glob(filepath.Join(base, "cmd", "safehome-*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		fset, files := parsePackage(t, dir)
		local := 0
		for _, f := range files {
			n, _ := brokerOptions(fset, f)
			local += n
		}
		program := filepath.Base(dir)
		name := cmp.Or(brokerNames[program], program)
		if local > 0 && !started[filepath.Join("cmd", program)+"/"+name] {
			t.Errorf("no test of cmd/%s runs it on mqtttest.Start(t, %q): a right missing from the ACL would pass unseen", program, name)
		}
	}
}

// The witness of testBrokers: open brokers are counted, and only a literal name of
// mqtttest.Start is taken.
func TestTestBrokersSeesOpenBrokersAndNames(t *testing.T) {
	_, f := parseSource(t, `package p
func a(t *testing.T) { mqtttest.StartOpen(t) }
func b(t *testing.T) { mqtttest.StartOpenWith(t, mqtttest.Options{TLS: &files}) }
func c(t *testing.T) { mqtttest.Start(t, "safehome-hub") }
func d(t *testing.T, name string) { mqtttest.Start(t, name) }
func e(t *testing.T) { other.StartOpen(t) }
`)
	open, names := testBrokers(f)
	if open != 2 || !slices.Equal(names, []string{"safehome-hub"}) {
		t.Fatalf("open %d, names %v: want 2 and [safehome-hub]", open, names)
	}
}
