package model_test

// deps_test.go — the executable guard for the canonical contract-types leaf. The model
// package holds the provider-agnostic Message/Block/Tool/Response/Usage types and the
// Provider interface — the vocabulary every other package speaks. It must import NO
// nilcore package: a dependency here would invert the whole graph (everything depends on
// model, so model must depend on nothing of ours). Stdlib-only keeps the contract types
// portable and the import graph rooted. This walks the full transitive closure.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestModelImportsNoNilcorePackage(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/model").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	for _, d := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		d = strings.TrimSpace(d)
		if d == "" || d == "nilcore/internal/model" {
			continue
		}
		if strings.HasPrefix(d, "nilcore/") {
			t.Errorf("the canonical contract-types leaf model must import NO nilcore package, found %q", d)
		}
	}
}
