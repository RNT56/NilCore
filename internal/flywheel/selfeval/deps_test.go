package selfeval_test

// deps_test.go — the executable LEAF guard (the trust / experience / blastbudget
// pattern).
//
// selfeval is a FOLD leaf: it takes a verifier-judged eval.Report, verifies the
// event-log chain, and folds the result into the trust scoreboard and the
// experience projection. It must NEVER import the orchestrator (agent / super /
// project) — doing so would invert the dependency direction (the flywheel wires
// the fold, never the reverse) and would let "what the agent eval'd itself at"
// reach back into "what is done", eroding the I2 boundary this package exists to
// protect. This test walks the full transitive import set via `go list -deps` and
// fails the build on any forbidden package. It lives in the external test package
// so it inspects the real dependency closure from outside.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestSelfEvalLeafDependencyClosure(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/flywheel/selfeval").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	// Forbidden ORCHESTRATOR packages: a leaf importing any of these inverts the
	// dependency direction. selfeval may import eval, trust, eventlog, and
	// experience (+ stdlib) — nothing in the orchestrator tier.
	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
	}
	have := map[string]bool{}
	for _, d := range deps {
		d = strings.TrimSpace(d)
		if reason, bad := forbidden[d]; bad {
			t.Errorf("selfeval leaf imports forbidden package %q (%s)", d, reason)
		}
		have[d] = true
	}

	// Positive assertion: the sanctioned reads must be in the closure, so a
	// refactor that silently drops one (e.g. stops verifying the chain) is visible
	// here. These are direct imports, so they are always in a healthy build's
	// closure.
	for _, w := range []string{
		"nilcore/eval",
		"nilcore/internal/trust",
		"nilcore/internal/eventlog",
	} {
		if !have[w] {
			t.Errorf("selfeval leaf is expected to import %q but it is absent from the closure", w)
		}
	}
}
