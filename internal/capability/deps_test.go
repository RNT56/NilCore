package capability_test

// deps_test.go — the executable LEAF guard.
//
// capability is a PURE leaf that computes a drive's permissions from policy /
// capguard / egressprofile (all leaves) + stdlib. It must NEVER import the
// orchestrator (agent / super / project), the model, the tool registry, or the
// session — any of those would invert the dependency direction (the call site
// computes a descriptor and wires the registry/gate, never the reverse). This
// test walks the full transitive import set via `go list -deps`.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestCapabilityLeafDependencyClosure(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/capability").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
		"nilcore/internal/model":   "model client",
		"nilcore/internal/tools":   "tool registry",
		"nilcore/internal/session": "session",
	}
	have := map[string]bool{}
	for _, d := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		d = strings.TrimSpace(d)
		if reason, bad := forbidden[d]; bad {
			t.Errorf("capability leaf imports forbidden package %q (%s)", d, reason)
		}
		have[d] = true
	}
	for _, w := range []string{
		"nilcore/internal/policy",
		"nilcore/internal/capguard",
		"nilcore/internal/egressprofile",
	} {
		if !have[w] {
			t.Errorf("capability leaf is expected to import %q but it is absent from the closure", w)
		}
	}
}
