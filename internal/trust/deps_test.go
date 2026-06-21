package trust_test

// deps_test.go — the executable LEAF guard.
//
// The trust ledger is a LEAF: it reads the event log, eval reports, the backend
// contract, and the terminal renderer, then ranks. It must NEVER import the
// orchestrator (agent / super / project) — doing so would invert the dependency
// direction (the orchestrator wires the leaf, never the reverse) and would let
// "who to try first" reach back into "what is done", eroding the I2 boundary this
// package is built to respect. This test walks the full transitive import set via
// `go list -deps` and fails the build on any forbidden package. It lives in the
// external test package so it inspects the real dependency closure from outside.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestTrustLeafDependencyClosure(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/trust").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	// Forbidden ORCHESTRATOR packages: a leaf importing any of these inverts the
	// dependency direction. The trust leaf may import backend, eventlog, eval, and
	// termui (+ stdlib) — nothing in the orchestrator tier.
	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
	}
	for _, d := range deps {
		if reason, bad := forbidden[d]; bad {
			t.Errorf("trust leaf imports forbidden package %q (%s)", d, reason)
		}
	}

	// Positive assertion: the closure should contain the sanctioned reads, so a
	// future refactor that silently drops one (e.g. stops reading the event log)
	// is at least visible here. These are direct imports, so they are always in
	// the closure of a healthy build.
	wantPresent := []string{
		"nilcore/internal/backend",
		"nilcore/internal/eventlog",
		"nilcore/eval",
		"nilcore/internal/termui",
	}
	have := map[string]bool{}
	for _, d := range deps {
		have[d] = true
	}
	for _, w := range wantPresent {
		if !have[w] {
			t.Errorf("trust leaf is expected to import %q but it is absent from the closure", w)
		}
	}
}
