package kernel_test

// deps_test.go — the executable LEAF guard (I1/I6). The unified kernel must import NO
// orchestration package (agent/session/project/swarm/backend) and NO module outside the
// standard library: the machines plug in as INJECTED closures the cmd layer wires, never
// the reverse. A nilcore import here would invert the dependency direction (the kernel is
// what entrypoints route THROUGH, it must not reach back into them) and let the recursive
// engine name a concrete machine. This walks the full transitive import set via
// `go list -deps` and fails the build on any forbidden import.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestKernelIsPureStdlibLeaf(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/kernel").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	for _, d := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		d = strings.TrimSpace(d)
		if d == "" || d == "nilcore/internal/kernel" {
			continue
		}
		if strings.HasPrefix(d, "nilcore/") {
			t.Errorf("kernel leaf must import NO nilcore package (the machines inject in), found %q", d)
		}
		// A non-stdlib module path carries a dotted domain in its first segment
		// (e.g. "golang.org/x/sys"). Stdlib import paths never do.
		if first := strings.SplitN(d, "/", 2)[0]; strings.Contains(first, ".") {
			t.Errorf("kernel leaf must be stdlib-only, found external module %q", d)
		}
	}
}
