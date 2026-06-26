package graapprove_test

// deps_test.go — the executable LEAF guard (I6, the trust/blastbudget pattern).
//
// graapprove is a policy LEAF: it reads the operator envelope, folds the
// append-only event log (read-only) into earned trust, consults the shared blast
// budget, and decides who-presses-the-button. It may import policy, eventlog,
// blastbudget (+ stdlib) only. It must NEVER reach into the orchestrator or the
// model/tool/session tiers — doing so would invert the dependency direction (the
// wiring layer installs the approver, never the reverse) and would let a routing
// decision reach back into the model surface, eroding the I2/I3 boundaries this
// package is built to respect. It must also add no Go module (I6). This test walks
// the full transitive import closure via `go list -deps` and fails the build on any
// forbidden import.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestGraApproveLeafDependencyClosure(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/graapprove").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	// Forbidden tiers: the orchestrator (importing it inverts direction) and the
	// model/tool/session surface (the envelope, trust, and blast state must never
	// reach the model — I3).
	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
		"nilcore/internal/model":   "model surface (I3)",
		"nilcore/internal/tools":   "tool surface (I3)",
		"nilcore/internal/session": "session surface (I3)",
	}

	// NOTE on modules: graapprove imports eventlog, whose SANCTIONED transitive
	// closure (the SQLite store backbone — CLAUDE.md §2 exception) legitimately
	// pulls modernc.org/sqlite + golang.org/x/sys. We therefore do NOT blanket-ban
	// dotted-domain paths (that is the right guard for a PURE stdlib leaf like
	// blastbudget, not for one that reads the event log). graapprove adds no module
	// of its OWN: go.mod is the authority on that, and any NEW dependency would show
	// up there in review. This guard's job is the orchestrator/model-tier boundary.
	have := map[string]bool{}
	for _, d := range deps {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		have[d] = true
		if reason, bad := forbidden[d]; bad {
			t.Errorf("graapprove leaf imports forbidden package %q (%s)", d, reason)
		}
	}

	// Positive assertion: the sanctioned reads are in the closure, so a refactor
	// that silently drops one is visible here.
	for _, w := range []string{
		"nilcore/internal/policy",
		"nilcore/internal/eventlog",
		"nilcore/internal/blastbudget",
	} {
		if !have[w] {
			t.Errorf("graapprove is expected to import %q but it is absent from the closure", w)
		}
	}
}
