package schema

import (
	"os/exec"
	"strings"
	"testing"
)

// TestLeafImports enforces the leaf rule structurally: the schema package must NOT
// pull the orchestrator into its dependency closure. It is the cheapest-first Named[0]
// in every pack's Composite, so it has to stay importable by the assembler without
// dragging super/agent/project/swarm/roster behind it (which would create an import
// cycle and defeat the "shape check is free" property). We assert over the FULL
// transitive closure via `go list -deps`, not just the direct imports, so a sneaky
// indirect import is caught too.
func TestLeafImports(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/artifact/schema").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := string(out)

	// The forbidden orchestrator packages. A leaf may import artifact, verify,
	// worktreefs, sandbox, and the standard library — never these.
	forbidden := []string{
		"nilcore/internal/super",
		"nilcore/internal/agent",
		"nilcore/internal/project",
		"nilcore/internal/swarm",
		"nilcore/internal/roster",
	}
	for _, f := range forbidden {
		for _, line := range strings.Split(deps, "\n") {
			if strings.TrimSpace(line) == f {
				t.Errorf("schema leaf must not import orchestrator package %q (full dep closure)", f)
			}
		}
	}
}
