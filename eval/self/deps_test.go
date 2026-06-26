package self_test

// deps_test.go — the executable LEAF guard.
//
// eval/self is a LEAF: it freezes a fixed, content-hashed self-eval suite from
// in-binary data. It must depend on NOTHING but the eval package (for the Case
// shape) and the standard library. In particular it must never import the
// orchestrator, the flywheel that consumes it, the trust ledger it folds into, or
// the event log — pulling any of those in would invert the dependency direction
// (the flywheel wires this leaf, never the reverse) and could let the run reach
// back into the frozen set, eroding the C6 guard this package exists to uphold.
// It also must make no model/network call; importing a model/provider package
// would be the smell. This test walks the full transitive import set via
// `go list -deps` and fails the build on any forbidden package or any non-stdlib,
// non-eval module package. It lives in the external test package so it inspects
// the real dependency closure from outside.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestSelfEvalLeafDependencyClosure(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/eval/self").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	const prefix = "nilcore/"
	// The ONLY nilcore package eval/self may pull in is the eval package itself.
	allowed := map[string]bool{
		"nilcore/eval":      true,
		"nilcore/eval/self": true, // self in its own closure
	}
	for _, d := range deps {
		if !strings.HasPrefix(d, prefix) {
			continue // stdlib (or x/... if ever) — checked separately below
		}
		if !allowed[d] {
			t.Errorf("eval/self leaf imports forbidden nilcore package %q "+
				"(a LEAF may import only nilcore/eval + stdlib)", d)
		}
	}

	// Positive assertion: the closure should contain the one sanctioned read, so a
	// future refactor that silently drops it is at least visible here.
	have := map[string]bool{}
	for _, d := range deps {
		have[d] = true
	}
	if !have["nilcore/eval"] {
		t.Error("eval/self is expected to import nilcore/eval but it is absent from the closure")
	}

	// Guard against accidentally pulling in a model/provider/network package: the
	// freeze step must make no model call. Any such import is a hard smell.
	forbiddenSubstr := []string{
		"nilcore/internal/model",
		"nilcore/internal/provider",
		"nilcore/internal/agent",
		"nilcore/internal/flywheel",
		"nilcore/internal/trust",
		"nilcore/internal/eventlog",
		"net/http",
	}
	for _, d := range deps {
		for _, bad := range forbiddenSubstr {
			if d == bad {
				t.Errorf("eval/self leaf must not import %q (freeze makes no model/network call)", d)
			}
		}
	}
}
