package distiller_test

// deps_test.go — the executable LEAF guard (I6, the trust pattern).
//
// The distiller is a LEAF: it REPLAYS the append-only event log read-only and
// clusters verifier-failure patterns into improvement targets. It may import
// internal/eventlog (and, transitively, internal/store — eventlog's sanctioned
// SQLite-backed second backing) and NOTHING in the orchestrator tier
// (agent / super / project / selfimprove). Importing the orchestrator would
// invert the dependency direction (the flywheel wires the distiller, never the
// reverse) and would let "what should we improve" reach back into "what is done",
// eroding the I2 boundary this package is built to respect.
//
// The zero-dependency core (I6) is enforced at the MODULE boundary: this leaf
// adds NO new Go module — its only non-stdlib closure is the SQLite driver pulled
// in transitively by internal/store, which CLAUDE.md §2 sanctions for the store.
// So the guard does NOT re-ban that closure (the trust leaf, which has the same
// eventlog→store→sqlite closure, doesn't either); it pins the dependency
// DIRECTION (no orchestrator) and asserts the sanctioned reads are present. A
// `go.mod` diff is the real I6 gate for a NEW module. This test lives in the
// external test package so it inspects the real dependency closure from outside.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDistillerLeafDependencyClosure(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/flywheel/distiller").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	// Forbidden ORCHESTRATOR / wiring packages: a leaf importing any of these
	// inverts the dependency direction. The distiller may read the event log
	// (and its store backing) — nothing in the orchestrator/wiring tier.
	forbidden := map[string]string{
		"nilcore/internal/agent":       "orchestrator (agent)",
		"nilcore/internal/super":       "orchestrator (super)",
		"nilcore/internal/project":     "orchestrator (project)",
		"nilcore/internal/selfimprove": "self-improve flow (wiring tier, not a leaf dep)",
	}

	have := map[string]bool{}
	for _, d := range deps {
		d = strings.TrimSpace(d)
		if d == "" || d == "nilcore/internal/flywheel/distiller" {
			continue // the package itself is always in its own dep list
		}
		have[d] = true
		if reason, bad := forbidden[d]; bad {
			t.Errorf("distiller leaf imports forbidden package %q (%s)", d, reason)
		}
	}

	// Positive assertion: the event log is the sanctioned read, so a future
	// refactor that silently stops replaying it (and therefore stops failing
	// closed on a broken chain) is at least visible here. It is a direct import,
	// so it is always in the closure of a healthy build.
	if !have["nilcore/internal/eventlog"] {
		t.Errorf("distiller leaf is expected to import %q but it is absent from the closure", "nilcore/internal/eventlog")
	}
}
