package selfacc_test

// deps_test.go — the executable LEAF guard (I6, the trust/blastbudget pattern).
//
// selfacc is a LEAF: it proposes acceptance criteria and authors candidate
// verifiers, reading only the data contracts it binds against — artifact,
// evverify, planner, sandbox (+ stdlib). It must NEVER import the orchestrator
// (agent / super / project): doing so would invert the dependency direction (the
// wiring layer installs the proposer/registry, never the reverse) and would let a
// "proposal" reach back into "what is done", eroding the I2 boundary this package
// exists to respect. This test walks the full transitive import set via
// `go list -deps` and fails the build on any forbidden package; it lives in the
// external test package so it inspects the real closure from outside.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestSelfAccLeafDependencyClosure(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/verify/selfacc").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	// Forbidden ORCHESTRATOR packages: a leaf importing any of these inverts the
	// dependency direction and lets proposal logic reach into shipping decisions.
	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
		"nilcore/internal/roster":  "orchestrator (roster)",
	}
	for _, d := range deps {
		d = strings.TrimSpace(d)
		if reason, bad := forbidden[d]; bad {
			t.Errorf("selfacc leaf imports forbidden package %q (%s)", d, reason)
		}
		// A non-stdlib module path carries a dotted domain in its first segment
		// (e.g. "golang.org/x/sys"). Stdlib import paths never do, and every
		// permitted nilcore import starts with "nilcore/". Anything else is a new
		// module dependency, which the zero-dependency core forbids (I6).
		if d == "" || strings.HasPrefix(d, "nilcore/") {
			continue
		}
		// golang.org/x/sys is the SANCTIONED §6/I6 exception (the namespace sandbox's
		// Landlock / no_new_privs / seccomp syscalls). selfacc legitimately imports
		// internal/sandbox (a sanctioned read — see wantPresent below, the I4 fix that
		// lets a self-authored verifier run ONLY sandboxed), and on Linux that sandbox
		// transitively pulls x/sys. It is therefore inherited, NOT a new module this
		// leaf adds — so allow it (mirrors internal/verify/vcache's deps_test). On
		// macOS the linux-only namespace backend isn't compiled, so x/sys is absent.
		if strings.HasPrefix(d, "golang.org/x/sys") {
			continue
		}
		if first := strings.SplitN(d, "/", 2)[0]; strings.Contains(first, ".") {
			t.Errorf("selfacc leaf must add no module dependency, found external module %q", d)
		}
	}

	// Positive assertion: the sanctioned reads must stay in the closure, so a
	// refactor that silently drops one is at least visible here.
	wantPresent := []string{
		"nilcore/internal/artifact",
		"nilcore/internal/artifact/evverify",
		"nilcore/internal/planner",
		"nilcore/internal/sandbox",
	}
	have := map[string]bool{}
	for _, d := range deps {
		have[strings.TrimSpace(d)] = true
	}
	for _, w := range wantPresent {
		if !have[w] {
			t.Errorf("selfacc leaf is expected to import %q but it is absent from the closure", w)
		}
	}
}
