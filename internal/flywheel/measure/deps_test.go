package measure_test

// deps_test.go — the executable LEAF guard (I6, the trust/blastbudget pattern).
//
// measure is the regression fence: it reads two eval.Reports and decides whether
// a self-improvement candidate measurably improved pass-rate. It is a LEAF — it
// may import nilcore/eval (the report type it measures) and the standard
// library, and NOTHING ELSE in the nilcore tree. In particular it must NEVER
// import the orchestrator (agent / super / project): doing so would invert the
// dependency direction (the wiring layer — selfimprove / flywheel loop —
// installs this fence, never the reverse) and would let "did it improve" reach
// back into "what is done", eroding the I2 boundary this fence exists to
// respect. This test walks the full transitive import set via `go list -deps`
// and fails the build on any forbidden import. It lives in the external test
// package so it inspects the real dependency closure from outside.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestMeasureLeafDependencyClosure(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/flywheel/measure").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	// The ONLY nilcore package this leaf is allowed to import is eval (plus the
	// package itself). Anything else under nilcore/ — and above all the
	// orchestrator tier — is forbidden, so a future edit that pulls the
	// orchestrator (or any other subsystem) in is caught at build time.
	const self = "nilcore/internal/flywheel/measure"
	allowed := map[string]bool{
		self:           true,
		"nilcore/eval": true,
	}
	for _, d := range deps {
		d = strings.TrimSpace(d)
		if d == "" || !strings.HasPrefix(d, "nilcore/") {
			continue
		}
		if !allowed[d] {
			t.Errorf("measure leaf must not import nilcore package %q (only nilcore/eval is permitted)", d)
		}
	}

	// Positive assertion: eval must be in the closure, so a refactor that
	// silently stops measuring the eval report (the whole point of the fence) is
	// at least visible here.
	have := map[string]bool{}
	for _, d := range deps {
		have[strings.TrimSpace(d)] = true
	}
	if !have["nilcore/eval"] {
		t.Errorf("measure leaf is expected to import %q but it is absent from the closure", "nilcore/eval")
	}
}
