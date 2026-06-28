package eventlog_test

// deps_test.go — the executable guard for the eventlog leaf (the append-only audit log,
// I5). It is the highest fan-in leaf — nearly everything appends to it — so it must
// never import the orchestrator: a dependency on agent/session/super/project/swarm would
// create a cycle (they all import eventlog) and invert the audit boundary. eventlog
// legitimately reaches the SQLite store (its optional second backing, P4-T02), which sits
// at/below the persistence tier, so the invariant is direction, not zero-imports.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestEventlogDoesNotImportOrchestrator(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/eventlog").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/session": "orchestrator (session)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
		"nilcore/internal/swarm":   "orchestrator (swarm)",
		"nilcore/internal/backend": "the backend contract (eventlog is below it)",
	}
	for _, d := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if reason, bad := forbidden[strings.TrimSpace(d)]; bad {
			t.Errorf("the eventlog leaf must not import %q (%s) — it would invert the audit boundary / create a cycle", d, reason)
		}
	}
}
