package trace_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestLeafDependencies asserts internal/trace stays a LEAF: it may import
// eventlog, termui, and the standard library, but NOT the orchestrator layer
// (agent / super / project) or anything that would import it back. This keeps
// the package-dependency direction defined in docs/ARCHITECTURE.md intact — a
// read-only viewer must never reach up into the loop it explains.
//
// The check shells out to `go list -deps` so it sees the real, transitive import
// graph the compiler builds, not just this package's direct imports.
func TestLeafDependencies(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/trace").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	forbidden := []string{
		"nilcore/internal/agent",
		"nilcore/internal/super",
		"nilcore/internal/project",
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep = strings.TrimSpace(dep)
		for _, bad := range forbidden {
			if dep == bad {
				t.Errorf("internal/trace must be a leaf but imports %q (transitively)", bad)
			}
		}
	}
}
