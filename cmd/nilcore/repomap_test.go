package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoMapShape builds a small tree and asserts the two-level map shows
// top-level dirs with second-level children and their source counts, lists
// root source files, and skips VCS/dependency dirs.
func TestRepoMapShape(t *testing.T) {
	root := t.TempDir()
	mk := func(parts ...string) {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("go.mod")
	mk("main.go")
	mk("internal", "agent", "orchestrator.go")
	mk("internal", "agent", "oracle.go")
	mk("internal", "verify", "verify.go")
	mk("docs", "ARCHITECTURE.md")
	mk(".git", "config")            // skipped
	mk("node_modules", "x", "y.js") // skipped

	got := repoMap(root, 0)
	for _, want := range []string{"internal/", "agent(2)", "verify(1)", "docs/", "go.mod", "main.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("map missing %q:\n%s", want, got)
		}
	}
	for _, banned := range []string{".git", "node_modules"} {
		if strings.Contains(got, banned) {
			t.Errorf("map must skip %q:\n%s", banned, got)
		}
	}
}

// TestRepoMapBudgetAndErrors pins the truncation marker and the error-degrade
// contract (missing root ⇒ empty string, never an error — orientation must not
// block a run).
func TestRepoMapBudgetAndErrors(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 60; i++ {
		d := filepath.Join(root, "pkg"+strings.Repeat("x", i%7)+string(rune('a'+i%26)))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "f.go"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := repoMap(root, 200)
	if len(got) > 300 {
		t.Errorf("map not bounded: %d bytes", len(got))
	}
	if !strings.Contains(got, "map truncated") {
		t.Errorf("truncated map must say so:\n%s", got)
	}
	if repoMap(filepath.Join(root, "missing"), 0) != "" {
		t.Error("missing root must degrade to empty, not error text")
	}
}
