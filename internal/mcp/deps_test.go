package mcp_test

// deps_test.go — the executable LEAF guard (I6). internal/mcp is the host-side MCP
// execution layer wired by cmd/nilcore; it must speak JSON-RPC over the STANDARD
// LIBRARY only (CLAUDE.md I6: "The MCP client is not a module — it speaks JSON-RPC over
// the standard library"), and must import NO nilcore package (a leaf the entrypoints
// consult, never one that reaches into the orchestrator). This walks the full transitive
// import set via `go list -deps` and fails the build on any nilcore import or any
// non-stdlib module — the same guard its peer leaves (router/kernel/model/…) carry, so a
// future edit that pulls in an HTTP/JSON-RPC module or an orchestrator package cannot
// silently breach I6 the way the eventlog→store / policy→blastbudget drifts once did.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestMCPIsPureStdlibLeaf(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/mcp").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	for _, d := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		d = strings.TrimSpace(d)
		if d == "" || d == "nilcore/internal/mcp" {
			continue
		}
		if strings.HasPrefix(d, "nilcore/") {
			t.Errorf("mcp leaf must import NO nilcore package, found %q", d)
		}
		if first := strings.SplitN(d, "/", 2)[0]; strings.Contains(first, ".") {
			t.Errorf("mcp leaf must be stdlib-only (I6), found external module %q", d)
		}
	}
}
