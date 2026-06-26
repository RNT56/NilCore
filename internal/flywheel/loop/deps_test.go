package loop_test

// deps_test.go — the executable LEAF guard (I6, the trust / distiller pattern).
//
// loop is the flywheel's WIRING leaf: it composes the lower flywheel leaves
// (selfeval/distiller/measure), the frozen self-eval suite (eval/self), the
// eval harness, and the gated self-edit flow (selfimprove) into one bounded
// standing cadence. Because it sits ABOVE those leaves it is allowed to import
// selfimprove (unlike the distiller, whose guard forbids it) — but it must
// still NEVER import the orchestrator/cmd tier (agent / super / project / cmd).
// Importing any of those would invert the dependency direction: the cmd layer
// wires and DRIVES the loop (Run is called under NILCORE_FLYWHEEL), never the
// reverse. Pulling the orchestrator in here would let "what should we improve"
// reach back into "what is done", eroding the I2 boundary the flywheel exists
// to respect, and would make the leaf un-testable without a model.
//
// As in the distiller's guard, the zero-dependency core (I6) is enforced at the
// MODULE boundary (a go.mod diff), not by re-banning the sanctioned
// eventlog→store→sqlite closure that selfimprove pulls in transitively. So this
// test pins the dependency DIRECTION (no orchestrator/cmd) and asserts the
// sanctioned composed leaves are present. It lives in the external test package
// so it inspects the real dependency closure from outside.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestLoopLeafDependencyClosure(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/flywheel/loop").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	// Forbidden ORCHESTRATOR / cmd packages: a leaf importing any of these
	// inverts the dependency direction. The loop composes the flywheel leaves
	// and the gated flow — nothing in the orchestrator/cmd tier may appear.
	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
	}
	have := map[string]bool{}
	for _, d := range deps {
		d = strings.TrimSpace(d)
		if d == "" || d == "nilcore/internal/flywheel/loop" {
			continue // the package itself is always in its own dep list
		}
		have[d] = true
		if reason, bad := forbidden[d]; bad {
			t.Errorf("loop leaf imports forbidden package %q (%s)", d, reason)
		}
		// The cmd tier is never a library dependency of a leaf: cmd wires the loop,
		// not the other way round. Catch any cmd/* import structurally.
		if strings.HasPrefix(d, "nilcore/cmd/") {
			t.Errorf("loop leaf imports forbidden cmd package %q (cmd wires the loop, never the reverse)", d)
		}
	}

	// Positive assertion: the sanctioned composed leaves must be in the closure,
	// so a refactor that silently drops one (e.g. stops distilling targets, or
	// stops proposing through the gated flow) is visible here. These are direct
	// imports, so they are always in a healthy build's closure.
	for _, w := range []string{
		"nilcore/eval",
		"nilcore/eval/self",
		"nilcore/internal/flywheel/distiller",
		"nilcore/internal/flywheel/measure",
		"nilcore/internal/selfimprove",
	} {
		if !have[w] {
			t.Errorf("loop leaf is expected to import %q but it is absent from the closure", w)
		}
	}
}
