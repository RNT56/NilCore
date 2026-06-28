package policy_test

// deps_test.go — the executable guard for the policy leaf (reversibility classifier +
// human gate + egress). It must never import the orchestrator: policy is consulted BY
// the loop/supervisor, it must not reach into them (that would invert direction and let
// the gate name a concrete machine). policy legitimately reaches blastbudget (the egress
// proxy's runtime fence), which is itself a pure leaf, so the invariant is direction.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestPolicyDoesNotImportOrchestrator(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/policy").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/session": "orchestrator (session)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
		"nilcore/internal/swarm":   "orchestrator (swarm)",
		"nilcore/internal/backend": "the backend contract (policy is below it)",
	}
	for _, d := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if reason, bad := forbidden[strings.TrimSpace(d)]; bad {
			t.Errorf("the policy leaf must not import %q (%s) — it would invert the dependency direction", d, reason)
		}
	}
}
