package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile is a small helper for staging a worktree fixture.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A query against a temp repo returns a structurally-coherent bundle: the lead
// symbol AND its call-graph neighborhood, each annotated with a provenance and a
// rationale (the bundle shape the understander reads to orient).
func TestCodeintelToolBundle(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "p.go", `package p
func leaf() int { return 1 }
func helper() int { return leaf() }
func Run() int { return helper() }
`)

	out, err := run(t, CodeintelTool{}, dir, `{"query":"Run"}`)
	if err != nil {
		t.Fatalf("codeintel: %v", err)
	}
	if !strings.Contains(out, "indexed 1 Go file") {
		t.Errorf("expected a 1-file index header, got:\n%s", out)
	}
	// The lead is present...
	if !strings.Contains(out, "Run") {
		t.Errorf("bundle should include the lead Run:\n%s", out)
	}
	// ...and its immediate neighborhood (Run calls helper) — structural coherence.
	if !strings.Contains(out, "helper") {
		t.Errorf("bundle should include the neighbor helper:\n%s", out)
	}
	// Every rendered item carries a provenance lens and a rationale ("— ...").
	if !strings.Contains(out, "[") || !strings.Contains(out, "] — ") {
		t.Errorf("items should carry provenance + rationale:\n%s", out)
	}
}

// The budget caps how many items the bundle returns.
func TestCodeintelToolBudget(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "p.go", `package p
func a() {}
func b() { a() }
func c() { b() }
func d() { c() }
func E() { d() }
`)
	out, err := run(t, CodeintelTool{}, dir, `{"query":"E","budget":2}`)
	if err != nil {
		t.Fatalf("codeintel: %v", err)
	}
	// Count the rendered item lines (each starts with "- ").
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "- ") {
			n++
		}
	}
	if n > 2 {
		t.Errorf("budget=2 but rendered %d items:\n%s", n, out)
	}
}

// Deterministic: the same worktree + query yields byte-identical output (the
// retriever orders deterministically; the file walk is sorted).
func TestCodeintelToolDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.go", "package p\nfunc Alpha() { Beta() }\n")
	writeFile(t, dir, "b.go", "package p\nfunc Beta() {}\n")

	o1, err := run(t, CodeintelTool{}, dir, `{"query":"Alpha"}`)
	if err != nil {
		t.Fatal(err)
	}
	o2, err := run(t, CodeintelTool{}, dir, `{"query":"Alpha"}`)
	if err != nil {
		t.Fatal(err)
	}
	if o1 != o2 {
		t.Errorf("nondeterministic output:\n--- 1 ---\n%s\n--- 2 ---\n%s", o1, o2)
	}
}

// A file that does not parse is skipped, never fatal — the bundle is built over
// whatever parses cleanly (best-effort indexing).
func TestCodeintelToolSkipsUnparseable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "good.go", "package p\nfunc Good() {}\n")
	writeFile(t, dir, "bad.go", "this is not go source at all {{{")

	out, err := run(t, CodeintelTool{}, dir, `{"query":"Good"}`)
	if err != nil {
		t.Fatalf("codeintel should not fail on an unparseable file: %v", err)
	}
	if !strings.Contains(out, "indexed 1 Go file") {
		t.Errorf("the unparseable file should be skipped (1 indexed), got:\n%s", out)
	}
}

// An empty query is rejected. A query with no lexical match still returns the
// repomap orientation (the central hubs) gracefully — never an error — so the
// understander always gets *some* footing in the repo.
func TestCodeintelToolEdgeCases(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "p.go", "package p\nfunc Only() {}\n")

	if _, err := run(t, CodeintelTool{}, dir, `{"query":"   "}`); err == nil {
		t.Error("empty query should be rejected")
	}
	out, err := run(t, CodeintelTool{}, dir, `{"query":"nonexistent_symbol_xyz"}`)
	if err != nil {
		t.Fatalf("a no-hit query should not error: %v", err)
	}
	// No lexical lead, but the repomap orientation still surfaces a central hub.
	if !strings.Contains(out, "Only") || !strings.Contains(out, "repomap") {
		t.Errorf("a no-hit query should still return the repomap orientation:\n%s", out)
	}
}

// A query against a worktree with NO Go files indexes nothing and returns the
// graceful empty-bundle note instead of erroring.
func TestCodeintelToolNoGoFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README.md", "# not go\n")

	out, err := run(t, CodeintelTool{}, dir, `{"query":"anything"}`)
	if err != nil {
		t.Fatalf("an empty repo should not error: %v", err)
	}
	if !strings.Contains(out, "indexed 0 Go file") || !strings.Contains(out, "no relevant symbols") {
		t.Errorf("expected an empty-bundle note over a Go-less repo:\n%s", out)
	}
}

// The rendered report never leaks an absolute host path: file paths are worktree-
// relative even though the in-memory graph records absolute paths.
func TestCodeintelToolReportIsWorktreeRelative(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pkg/p.go", "package p\nfunc Target() {}\n")

	out, err := run(t, CodeintelTool{}, dir, `{"query":"Target"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, dir) {
		t.Errorf("report leaked the absolute worktree path %q:\n%s", dir, out)
	}
	if !strings.Contains(out, "pkg/p.go") {
		t.Errorf("report should carry the worktree-relative path pkg/p.go:\n%s", out)
	}
}

// The codeintel tool advertises a stable schema and name (it is dispatched by name
// through the registry like every other structured tool).
func TestCodeintelToolSchema(t *testing.T) {
	var tool Tool = CodeintelTool{}
	if tool.Name() != "codeintel" {
		t.Errorf("name = %q, want codeintel", tool.Name())
	}
	if !json.Valid(tool.Schema()) {
		t.Errorf("schema is not valid JSON: %s", tool.Schema())
	}
	if !strings.Contains(string(tool.Schema()), "query") {
		t.Errorf("schema should declare a query field: %s", tool.Schema())
	}
}

// Sanity: indexing honors context cancellation (a cancelled ctx aborts the walk).
func TestCodeintelToolHonorsContext(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "p.go", "package p\nfunc Run() {}\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := CodeintelTool{}
	if _, err := tool.Run(ctx, dir, json.RawMessage(`{"query":"Run"}`)); err == nil {
		t.Error("a cancelled context should abort the call")
	}
}
