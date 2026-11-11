package qualitytest

import (
	"go/ast"
	"slices"
	"testing"
)

// A TLS client that skips the certificate's check talks to anyone who answers: the
// cloud link, the photos and the tunnel all rest on that check. No file of the
// module, tests included, may turn it off: a test talks to httptest's own client,
// which trusts its server.
var tlsRule = rule{
	name: "tls-verify",
	fix:  "keep the certificate check; in a test, use the httptest server's Client() or add its certificate to RootCAs",
}

// skippedVerifies returns every place that names tls.Config's InsecureSkipVerify, as a
// field of a literal or through a selector.
func skippedVerifies(tr *tree) []finding {
	var found []finding
	for _, f := range tr.files {
		ast.Inspect(f.ast, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "InsecureSkipVerify" {
				found = append(found, finding{pos: tr.pos(id), key: f.rel, msg: "the TLS certificate check is turned off"})
			}
			return true
		})
	}
	return found
}

func TestNoTLSCertificateCheckIsSkipped(t *testing.T) {
	tlsRule.enforce(t, skippedVerifies(moduleTree(t)))
}

func TestTLSRuleCatchesViolations(t *testing.T) {
	tr := sample(t, map[string]string{
		"internal/adapter/up/u.go": "package up\nimport \"crypto/tls\"\nvar c = &tls.Config{InsecureSkipVerify: true}\n",
		"internal/app/a_test.go":   "package app\nimport \"crypto/tls\"\nfunc f(c *tls.Config) { c.InsecureSkipVerify = true }\n",
		"internal/app/ok.go":       "package app\nimport \"crypto/tls\"\nvar c = &tls.Config{MinVersion: tls.VersionTLS12}\n",
	})
	got := keys(skippedVerifies(tr))
	slices.Sort(got)
	if !slices.Equal(got, []string{"internal/adapter/up/u.go", "internal/app/a_test.go"}) {
		t.Errorf("tls rule: got %v", got)
	}
}
