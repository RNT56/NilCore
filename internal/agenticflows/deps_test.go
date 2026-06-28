package agenticflows_test

// deps_test.go — the executable leaf guard for the agentic-flows adapter (added with
// the consumer wiring). It maps a decoded flow onto NilCore's spawn + sandbox seams and
// must stay a leaf: it imports spawn/sandbox/summarize/model/planner/scheduler/blastbudget
// (all downward) and must NEVER import the orchestrator — the cmd layer (nilcore flows)
// drives it, not the reverse. Every other Phase-16 leaf carries this guard; this closes
// the one the remediation introduced without one.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestAgenticflowsDoesNotImportOrchestrator(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/agenticflows").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/session": "orchestrator (session)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
		"nilcore/internal/swarm":   "orchestrator (swarm)",
	}
	for _, d := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if reason, bad := forbidden[strings.TrimSpace(d)]; bad {
			t.Errorf("the agenticflows leaf must not import %q (%s) — it would invert the dependency direction", d, reason)
		}
	}
}
