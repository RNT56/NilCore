package lessons_test

// deps_test.go — the executable LEAF guard (the trust/blastbudget pattern).
//
// lessons is a LEAF: it READS the event log and emits memory.Record values, so it
// may import ONLY internal/eventlog, internal/memory, and their closures (+ the
// standard library). It must NEVER import the orchestrator (agent / super /
// project) — doing so would invert the dependency direction (the wiring layer in
// LRN-T03 installs this distiller, never the reverse) and would let a "lesson"
// reach back into "what is done", eroding the I2 boundary. This test walks the
// full transitive import set via `go list -deps` and fails the build on any
// forbidden package. It lives in the external test package so it inspects the real
// dependency closure from outside.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestLessonsLeafDependencyClosure(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/memory/lessons").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	// Forbidden ORCHESTRATOR packages: a leaf importing any of these inverts the
	// dependency direction. The lessons leaf may import eventlog, memory (+ store,
	// pulled in transitively by memory) and stdlib — nothing in the orchestrator
	// tier.
	forbidden := map[string]string{
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/project": "orchestrator (project)",
	}
	for _, d := range deps {
		if reason, bad := forbidden[strings.TrimSpace(d)]; bad {
			t.Errorf("lessons leaf imports forbidden package %q (%s)", d, reason)
		}
	}

	// Positive assertion: the sanctioned reads must stay in the closure, so a future
	// refactor that silently drops one (e.g. stops verifying the chain, or stops
	// emitting memory records) is at least visible here.
	wantPresent := []string{
		"nilcore/internal/eventlog",
		"nilcore/internal/memory",
	}
	have := map[string]bool{}
	for _, d := range deps {
		have[strings.TrimSpace(d)] = true
	}
	for _, w := range wantPresent {
		if !have[w] {
			t.Errorf("lessons leaf is expected to import %q but it is absent from the closure", w)
		}
	}
}
