package experience_test

// deps_test.go — the executable LEAF guard (the trust/swarm pattern).
//
// experience is a read leaf: it folds the event log, the trust scoreboard, eval
// rollups, and memory into one Reader. It must NEVER import the orchestrator
// (agent / super / project) — doing so would invert the dependency direction
// (the orchestrator wires the reader, never the reverse) and would let "what the
// agent has learned" reach back into "what is done", eroding the I2 boundary
// this package is built to respect. This test walks the full transitive import
// set via `go list -deps` and fails the build on any forbidden package.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestExperienceLeafDependencyClosure(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/experience").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
	}
	have := map[string]bool{}
	for _, d := range deps {
		if reason, bad := forbidden[strings.TrimSpace(d)]; bad {
			t.Errorf("experience leaf imports forbidden package %q (%s)", d, reason)
		}
		have[strings.TrimSpace(d)] = true
	}

	// Positive assertion: the sanctioned reads must be in the closure, so a
	// refactor that silently drops one is visible here.
	for _, w := range []string{
		"nilcore/internal/eventlog",
		"nilcore/internal/trust",
		"nilcore/internal/memory",
	} {
		if !have[w] {
			t.Errorf("experience leaf is expected to import %q but it is absent from the closure", w)
		}
	}
}
