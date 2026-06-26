package blastbudget_test

// deps_test.go — the executable LEAF guard (I6, the capguard/trust pattern).
//
// blastbudget is a PURE leaf: it meters capability axes and emits through a Sink
// interface, so it must import NO nilcore package and NO module outside the
// standard library. A nilcore import would invert the dependency direction (the
// wiring layer installs the budget, never the reverse) and a module import would
// breach the zero-dependency core. This test walks the full transitive import
// set via `go list -deps` and fails the build on any forbidden import.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestBlastBudgetIsPureStdlibLeaf(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/blastbudget").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	for _, d := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		d = strings.TrimSpace(d)
		if d == "" || d == "nilcore/internal/blastbudget" {
			continue // the package itself is always in its own dep list
		}
		if strings.HasPrefix(d, "nilcore/") {
			t.Errorf("blastbudget leaf must import no nilcore package, found %q", d)
		}
		// A non-stdlib module path contains a dotted domain in its first segment
		// (e.g. "golang.org/x/sys"). Stdlib import paths never do.
		if first := strings.SplitN(d, "/", 2)[0]; strings.Contains(first, ".") {
			t.Errorf("blastbudget leaf must be stdlib-only, found external module %q", d)
		}
	}
}
