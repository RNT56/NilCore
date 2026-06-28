package backend_test

// deps_test.go — the executable guard for the FROZEN CONTRACT leaf (I1). The
// backend package declares `CodingBackend = Run(ctx, Task) (Result, error)` and the
// optional native-loop seams (Advisor/Peer/Inbox/Emitter/...). Those seams are declared
// as INTERFACES here precisely so backend never imports the orchestrator: the cmd layer
// injects concrete closures, the orchestrator wires `backend`, never the reverse. An
// import of internal/agent (or any higher orchestration package) would invert the
// dependency direction and let the frozen contract reach back into the machinery that
// routes through it. The contracts review found this guard missing; without it the
// eventlog→store and policy→blastbudget drifts landed unobserved (see the sibling
// leaves). This walks the full transitive closure via `go list -deps`.
//
// NOTE: backend is NOT a pure-stdlib leaf — it legitimately imports the native loop's
// lower-level machinery (model/sandbox/verify/eventlog/tools/advisor/emit/guard/
// loopctl/summarize). The invariant is direction (no orchestrator), not zero-imports.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestBackendDoesNotImportOrchestrator(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/backend").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/session": "orchestrator (session)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
		"nilcore/internal/swarm":   "orchestrator (swarm)",
		"nilcore/internal/kernel":  "orchestration kernel",
	}
	for _, d := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if reason, bad := forbidden[strings.TrimSpace(d)]; bad {
			t.Errorf("the frozen-contract leaf backend must not import %q (%s) — it would invert the dependency direction (I1)", d, reason)
		}
	}
}
