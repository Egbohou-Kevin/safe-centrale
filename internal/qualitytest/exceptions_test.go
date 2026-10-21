package qualitytest

import "testing"

// exception lets one place break one rule, for a reason said here. The key names the
// place as the rule does: a file ("internal/x/y.go"), a function or type
// ("internal/x/y.go:Name", "internal/x/y.go:Type.Method"), an identifier for the dead
// code rule ("internal/x.Name"), or "copy~original" files for the duplication rule.
type exception struct {
	rule string
	key  string
	why  string
}

// allowed is the whole list of exceptions. Keep it short: fix the code first, and add
// an entry only when the rule is wrong for that place, with the reason.
var allowed = []exception{
	{
		rule: "duplication",
		key:  "cmd/safehome-video/run.go~cmd/safehome-gsm/run.go",
		why: "the two programs that restart for a new configuration from the cloud wire the same daemon " +
			"(one application, one broker, config.applied); the logic is shared (service.Restart.Watch, " +
			"siteconfig.Changed), and sharing 11 lines of wiring would need an interface over two applications",
	},
	{
		rule: "test-duplication",
		key:  "cmd/safehome-video/run_test.go~cmd/safehome-gsm/run_test.go",
		why: "both programs' tests play the hub announcing a configuration (publishApplied); tests of two " +
			"main packages cannot share code, and mqtttest cannot hold it: the mqtt adapter's own tests " +
			"import mqtttest, so mqtttest cannot import the adapter's client",
	},
}

func TestEveryExceptionHasARuleAndAReason(t *testing.T) {
	known := map[string]bool{}
	for _, r := range []rule{sizeRule, gofmtRule, asciiRule, duplicationRule, testDuplicationRule, fakesRule, deadRule,
		slogRule, sleepRule, markerRule, packageDocRule, readmeRule} {
		known[r.name] = true
	}
	for _, e := range allowed {
		if !known[e.rule] || e.key == "" || len(e.why) < 20 {
			t.Errorf("exception %+v: it needs a known rule, a key and a real reason", e)
		}
	}
}
