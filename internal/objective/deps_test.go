package objective_test

// deps_test.go — the executable LEAF guard (I6, the capguard/trust/blastbudget pattern).
//
// objective is a PURE leaf: it owns the standing-objectives selection policy over a
// narrow Store seam (satisfied later by *store.Store), so it must import NO nilcore
// package and NO module outside the standard library. A nilcore import — especially
// internal/store — would invert the dependency direction (the wiring layer installs the
// store into the backlog, never the reverse) and a module import would breach the
// zero-dependency core. This test walks the full transitive import set via
// `go list -deps` and fails the build on any forbidden import.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestObjectiveIsPureStdlibLeaf(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/objective").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	for _, d := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		d = strings.TrimSpace(d)
		if d == "" || d == "nilcore/internal/objective" {
			continue // the package itself is always in its own dep list
		}
		if strings.HasPrefix(d, "nilcore/") {
			t.Errorf("objective leaf must import no nilcore package, found %q", d)
		}
		// A non-stdlib module path has a dotted domain in its first segment
		// (e.g. "golang.org/x/sys"); stdlib import paths never do.
		if first := strings.SplitN(d, "/", 2)[0]; strings.Contains(first, ".") {
			t.Errorf("objective leaf must be stdlib-only, found external module %q", d)
		}
	}
}
