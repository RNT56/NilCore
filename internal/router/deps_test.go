package router_test

// deps_test.go — the executable LEAF guard (I1/I6). The preset router must import NO
// orchestration package (kernel/agent/session/project/swarm/backend) and NO module
// outside the standard library: it maps a goal to a Preset NAME, and the cmd layer maps
// that name onto a proven machine. A nilcore import here would invert the dependency
// direction (the router is a pure classifier the entrypoints consult, it must not reach
// into them) and let routing entangle with execution. This walks the full transitive
// import set via `go list -deps` and fails the build on any forbidden import.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRouterIsPureStdlibLeaf(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/router").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	for _, d := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		d = strings.TrimSpace(d)
		if d == "" || d == "nilcore/internal/router" {
			continue
		}
		if strings.HasPrefix(d, "nilcore/") {
			t.Errorf("router leaf must import NO nilcore package, found %q", d)
		}
		if first := strings.SplitN(d, "/", 2)[0]; strings.Contains(first, ".") {
			t.Errorf("router leaf must be stdlib-only, found external module %q", d)
		}
	}
}
