package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"safehome/internal/contract"
)

// carriesCode tells whether messages of a type may carry the site's access code: its
// JSON Schema (schemas/) has a property named code, at any depth. The schemas are the
// contract the cloud copies, so a new field carrying a code shows there first.
func carriesCode(t *testing.T, typ string) bool {
	t.Helper()
	file := strings.NewReplacer(".", "-", "_", "-").Replace(typ) + ".json"
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", file))
	if err != nil {
		t.Fatalf("the schema of %s: %v", typ, err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return hasCodeProperty(doc)
}

func hasCodeProperty(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if props, ok := x["properties"].(map[string]any); ok && props["code"] != nil {
			return true
		}
		for _, child := range x {
			if hasCodeProperty(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(x, hasCodeProperty)
	}
	return false
}

// codeMismatches lists the routes whose code flag (QoS 0, nothing kept by a broker)
// disagrees with what their types may carry.
func codeMismatches(t *testing.T, routes map[string]downRoute) []string {
	t.Helper()
	var bad []string
	for key, route := range routes {
		if slices.ContainsFunc(route.types, func(typ string) bool { return carriesCode(t, typ) }) != route.code {
			bad = append(bad, key)
		}
	}
	slices.Sort(bad)
	return bad
}

// The topics read at QoS 0 are exactly those whose messages may carry the access
// code: a new type with a code on a QoS 1 topic would wait on the cloud's broker for
// an absent agent, and a topic at QoS 0 without one would lose what comes during an
// outage for nothing.
func TestTheTopicsReadAtQoS0AreThoseThatMayCarryTheCode(t *testing.T) {
	if bad := codeMismatches(t, downRoutes); len(bad) > 0 {
		t.Errorf("the code flag of %v does not match the schemas of their types", bad)
	}
	// Witness: the rule sees the code where it is, and catches both mistakes.
	if !carriesCode(t, contract.TypeAlarmCommand) || !carriesCode(t, contract.TypeDeviceEvent) || carriesCode(t, contract.TypeVideoRequest) {
		t.Fatal("carriesCode does not read the schemas right")
	}
	witness := map[string]downRoute{
		"alarm/cmd": {types: []string{contract.TypeAlarmCommand}},
		"video/cmd": {types: []string{contract.TypeVideoRequest}, code: true},
		"maint/cmd": {types: []string{contract.TypeMaintOpen}},
	}
	if got := codeMismatches(t, witness); !slices.Equal(got, []string{"alarm/cmd", "video/cmd"}) {
		t.Fatalf("the rule caught %v", got)
	}
}
