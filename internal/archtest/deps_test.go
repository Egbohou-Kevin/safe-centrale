package archtest

import (
	"go/ast"
	"path/filepath"
	"slices"
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
// internal/outbox/outboxtest the broker: shipped code must never import them.
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
