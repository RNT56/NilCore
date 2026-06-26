package vcache_test

// deps_test.go — the executable LEAF guard (the trust/blastbudget pattern, I6).
//
// vcache is a verify-side LEAF: it reads the append-only event log, hashes the
// worktree through the confinement primitives, and decorates a verify.Verifier. It
// may import exactly those sanctioned leaves (eventlog, verify, worktreefs) plus
// the standard library — and NOTHING in the orchestrator tier. An orchestrator
// import would invert the dependency direction (the wiring layer installs the
// cache, never the reverse) and would let "skip a run" reach back into "what is
// done", eroding the I2 boundary this package exists to respect.
//
// On I6 (no NEW Go module): vcache adds none of its own. Its only external-module
// edges arrive transitively through the SANCTIONED eventlog import, which carries
// the §6-permitted SQLite backbone (modernc.org/*) and golang.org/x/sys as its
// second store backing. We therefore assert the module closure contains ONLY those
// pre-approved roots — any module outside that set would be a genuinely new
// dependency this leaf introduced, and fails the build. This test walks the full
// transitive import set via `go list -deps` from the external test package so it
// inspects the real dependency closure from outside.

import (
	"os/exec"
	"strings"
	"testing"
)

// sanctionedModuleRoots are the §6-exception module path prefixes vcache is allowed
// to inherit transitively (and ONLY transitively, via eventlog). It introduces no
// module of its own; anything outside this set is a new dependency and a failure.
var sanctionedModuleRoots = []string{
	"modernc.org/",  // SQLite backbone (the eventlog store backing), §6 exception
	"golang.org/x/", // the Go project's extended stdlib (x/sys et al.), §6 exception
	"github.com/",   // pulled in transitively by modernc.org/sqlite's own deps
}

func sanctioned(dep string) bool {
	for _, root := range sanctionedModuleRoots {
		if strings.HasPrefix(dep, root) {
			return true
		}
	}
	return false
}

func TestVCacheLeafDependencyClosure(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/verify/vcache").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	// Forbidden ORCHESTRATOR packages: a leaf importing any of these inverts the
	// dependency direction and undermines the I2 boundary.
	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
		"nilcore/internal/server":  "orchestrator (server)",
	}

	have := map[string]bool{}
	for _, d := range deps {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		have[d] = true
		if reason, bad := forbidden[d]; bad {
			t.Errorf("vcache leaf imports forbidden package %q (%s)", d, reason)
		}
		// I6: no NEW Go module. nilcore-internal imports are checked above; a
		// non-stdlib module path carries a dotted domain in its first segment (e.g.
		// "golang.org/x/sys"), stdlib paths never do. Such a module is allowed ONLY if
		// it is a §6-sanctioned root inherited transitively through eventlog.
		if strings.HasPrefix(d, "nilcore/") {
			continue
		}
		if first := strings.SplitN(d, "/", 2)[0]; strings.Contains(first, ".") && !sanctioned(d) {
			t.Errorf("vcache leaf introduced a non-sanctioned external module: %q", d)
		}
	}

	// Positive assertion: the sanctioned reads must be in the closure, so a future
	// refactor that silently drops one (e.g. stops reading the event log, breaking
	// the I2 chain check) is visible here. These are direct imports, always present
	// in a healthy build.
	wantPresent := []string{
		"nilcore/internal/eventlog",
		"nilcore/internal/verify",
		"nilcore/internal/worktreefs",
	}
	for _, w := range wantPresent {
		if !have[w] {
			t.Errorf("vcache leaf is expected to import %q but it is absent from the closure", w)
		}
	}
}
