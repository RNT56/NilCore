package tools

// hosttools_test.go exercises the additional host-side structural tools end to end
// against a temporary worktree: outline, read_symbol, dead_code, import_graph,
// format_file, edit_checked, patch, plan, git blame/show, affected_tests,
// rename_symbol, structural_replace. The tests are hermetic (a t.TempDir worktree,
// real git only where a repo is genuinely needed) and assert behaviour at the tool
// boundary (Run input → output string / file effects), matching the existing
// tools_test.go style.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runTool(t *testing.T, tool Tool, dir string, in map[string]any) string {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	out, err := tool.Run(context.Background(), dir, b)
	if err != nil {
		t.Fatalf("%s.Run(%v): %v", tool.Name(), in, err)
	}
	return out
}

func runToolErr(t *testing.T, tool Tool, dir string, in map[string]any) (string, error) {
	t.Helper()
	b, _ := json.Marshal(in)
	return tool.Run(context.Background(), dir, b)
}

func mkFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readF(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func goModule(t *testing.T) string {
	dir := t.TempDir()
	mkFile(t, dir, "go.mod", "module testmod\n\ngo 1.25\n")
	return dir
}

// TestNewToolContracts locks the model-facing contract for every new host-side
// tool: a non-empty name/description and a Schema() that is valid JSON (a malformed
// schema would only fail at advertisement time, never at build).
func TestNewToolContracts(t *testing.T) {
	newTools := []Tool{
		OutlineTool{}, ReadSymbolTool{}, DeadCodeTool{}, ImportGraphTool{},
		FormatTool{}, EditCheckedTool{}, PatchTool{}, PlanTool{},
		AffectedTestsTool{}, RenameSymbolTool{}, StructuralReplaceTool{},
	}
	for _, tool := range newTools {
		if tool.Name() == "" || tool.Description() == "" {
			t.Errorf("%T has empty name/description", tool)
		}
		if !json.Valid(tool.Schema()) {
			t.Errorf("%s.Schema() is not valid JSON: %s", tool.Name(), tool.Schema())
		}
	}
}

func TestOutlineFileAndDir(t *testing.T) {
	dir := goModule(t)
	mkFile(t, dir, "pkg/a.go", "package pkg\n\ntype T struct{}\n\nfunc (t T) M() {}\n\nfunc F() {}\n")
	mkFile(t, dir, "pkg/b.go", "package pkg\n\nfunc g() {}\n")

	out := runTool(t, OutlineTool{}, dir, map[string]any{"path": "pkg/a.go"})
	for _, want := range []string{"type T", "method", "F", "(T) M"} {
		if !strings.Contains(out, want) {
			t.Errorf("outline file missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "*func F") { // exported star
		t.Errorf("expected exported star on F:\n%s", out)
	}

	outDir := runTool(t, OutlineTool{}, dir, map[string]any{"path": "pkg"})
	if !strings.Contains(outDir, "a.go") || !strings.Contains(outDir, "b.go") {
		t.Errorf("outline dir missing files:\n%s", outDir)
	}
}

func TestReadSymbol(t *testing.T) {
	dir := goModule(t)
	mkFile(t, dir, "a.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() {}\n")

	out := runTool(t, ReadSymbolTool{}, dir, map[string]any{"name": "Alpha"})
	if !strings.Contains(out, "func Alpha() int") || !strings.Contains(out, "return 1") {
		t.Errorf("read_symbol Alpha missing body:\n%s", out)
	}
	if !strings.Contains(out, "a.go:L3-5") {
		t.Errorf("expected span header:\n%s", out)
	}

	miss := runTool(t, ReadSymbolTool{}, dir, map[string]any{"name": "Nope"})
	if !strings.Contains(miss, "not found") {
		t.Errorf("expected not-found:\n%s", miss)
	}

	// Ambiguity across files.
	mkFile(t, dir, "b.go", "package p\n\nfunc Dup() {}\n")
	mkFile(t, dir, "c.go", "package p\n\nfunc Dup() {}\n")
	amb := runTool(t, ReadSymbolTool{}, dir, map[string]any{"name": "Dup"})
	if !strings.Contains(amb, "ambiguous") {
		t.Errorf("expected ambiguity:\n%s", amb)
	}
}

func TestDeadCode(t *testing.T) {
	dir := goModule(t)
	mkFile(t, dir, "a.go", "package p\n\nfunc Used() { helper() }\n\nfunc helper() {}\n\nfunc orphan() {}\n")
	out := runTool(t, DeadCodeTool{}, dir, map[string]any{})
	if !strings.Contains(out, "orphan") {
		t.Errorf("expected orphan flagged dead:\n%s", out)
	}
	if strings.Contains(out, " helper ") || strings.Contains(out, "Used") {
		t.Errorf("reachable funcs should not be flagged:\n%s", out)
	}
}

func TestImportGraphCyclesLayersDeps(t *testing.T) {
	dir := goModule(t)
	mkFile(t, dir, "a/a.go", "package a\n\nimport _ \"testmod/b\"\n")
	mkFile(t, dir, "b/b.go", "package b\n\nimport _ \"testmod/a\"\n")

	cyc := runTool(t, ImportGraphTool{}, dir, map[string]any{"op": "cycles"})
	if !strings.Contains(cyc, "1 cycle") {
		t.Errorf("expected a cycle:\n%s", cyc)
	}

	lay := runTool(t, ImportGraphTool{}, dir, map[string]any{"op": "layers", "layers": []string{"testmod/a", "testmod/b"}})
	// a (high) → b (low) is fine; b (low) → a (high) is a violation.
	if !strings.Contains(lay, "testmod/b → testmod/a") {
		t.Errorf("expected layering violation b→a:\n%s", lay)
	}

	deps := runTool(t, ImportGraphTool{}, dir, map[string]any{"op": "deps", "pkg": "a"})
	if !strings.Contains(deps, "testmod/b") {
		t.Errorf("expected a to depend on b:\n%s", deps)
	}
}

func TestFormatFile(t *testing.T) {
	dir := goModule(t)
	mkFile(t, dir, "a.go", "package p\nfunc  F(){\nx:=1\n_=x}\n") // deliberately misformatted
	out := runTool(t, FormatTool{}, dir, map[string]any{"path": "a.go"})
	if !strings.Contains(out, "reformatted") {
		t.Errorf("expected reformat:\n%s", out)
	}
	got := readF(t, dir, "a.go")
	if !strings.Contains(got, "func F() {") {
		t.Errorf("not gofmt'd:\n%s", got)
	}

	// Unparseable Go fails soft (no write, no error).
	mkFile(t, dir, "bad.go", "package p\nfunc F( {\n")
	soft := runTool(t, FormatTool{}, dir, map[string]any{"path": "bad.go"})
	if !strings.Contains(soft, "does not parse") {
		t.Errorf("expected fail-soft:\n%s", soft)
	}
}

func TestEditChecked(t *testing.T) {
	dir := goModule(t)
	mkFile(t, dir, "a.go", "package p\n\nfunc F() {}\n")

	// Valid edit accepted.
	ok := runTool(t, EditCheckedTool{}, dir, map[string]any{"path": "a.go", "old": "func F() {}", "new": "func F() { _ = 1 }"})
	if !strings.Contains(ok, "parse ok") {
		t.Errorf("expected parse ok:\n%s", ok)
	}

	// Syntax-breaking edit rejected, file unchanged.
	before := readF(t, dir, "a.go")
	_, err := runToolErr(t, EditCheckedTool{}, dir, map[string]any{"path": "a.go", "old": "func F() { _ = 1 }", "new": "func F() { _ = 1 "})
	if err == nil || !strings.Contains(err.Error(), "REJECTED") {
		t.Errorf("expected rejection, got err=%v", err)
	}
	if readF(t, dir, "a.go") != before {
		t.Errorf("file should be unchanged after rejection")
	}
}

func TestPatchAtomic(t *testing.T) {
	dir := goModule(t)
	mkFile(t, dir, "f.txt", "alpha\nbeta\ngamma\n")

	// Successful multi-op: update one file + add another.
	out := runTool(t, PatchTool{}, dir, map[string]any{"ops": []map[string]any{
		{"kind": "update_file", "path": "f.txt", "hunks": []map[string]any{
			{"context_before": []string{"alpha"}, "removed": []string{"beta"}, "added": []string{"BETA"}, "context_after": []string{"gamma"}},
		}},
		{"kind": "add_file", "path": "g.txt", "content": "new\n"},
	}})
	if !strings.Contains(out, "applied 2 op") {
		t.Errorf("expected 2 ops applied:\n%s", out)
	}
	if got := readF(t, dir, "f.txt"); !strings.Contains(got, "BETA") {
		t.Errorf("hunk not applied:\n%s", got)
	}
	if readF(t, dir, "g.txt") != "new\n" {
		t.Errorf("add_file not applied")
	}

	// Atomic failure: a valid op + an invalid op (add over existing) ⇒ nothing written.
	before := readF(t, dir, "f.txt")
	_, err := runToolErr(t, PatchTool{}, dir, map[string]any{"ops": []map[string]any{
		{"kind": "update_file", "path": "f.txt", "content": "WHOLE\n"},
		{"kind": "add_file", "path": "g.txt", "content": "x"}, // already exists ⇒ fail
	}})
	if err == nil {
		t.Fatalf("expected validation failure")
	}
	if readF(t, dir, "f.txt") != before {
		t.Errorf("f.txt mutated despite atomic failure")
	}
}

func TestPlan(t *testing.T) {
	dir := t.TempDir()
	runTool(t, PlanTool{}, dir, map[string]any{"op": "set", "steps": []map[string]any{
		{"id": "a", "title": "first", "status": "active"},
		{"id": "b", "title": "second", "status": "active"}, // second active demoted
	}})
	out := runTool(t, PlanTool{}, dir, map[string]any{"op": "get"})
	if strings.Count(out, "[~]") != 1 {
		t.Errorf("expected exactly one active step:\n%s", out)
	}
	runTool(t, PlanTool{}, dir, map[string]any{"op": "patch", "id": "a", "status": "done"})
	out = runTool(t, PlanTool{}, dir, map[string]any{"op": "get"})
	if !strings.Contains(out, "[x] a first") {
		t.Errorf("expected a done:\n%s", out)
	}
}

func TestGitBlameShow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := goModule(t)
	mkFile(t, dir, "a.go", "package p\n\nfunc F() {}\n")
	gitCommit(t, dir)

	blame := runTool(t, GitTool{}, dir, map[string]any{"op": "blame", "path": "a.go"})
	if !strings.Contains(blame, "func F()") {
		t.Errorf("blame missing source:\n%s", blame)
	}
	show := runTool(t, GitTool{}, dir, map[string]any{"op": "show"})
	if !strings.Contains(show, "a.go") {
		t.Errorf("show missing file:\n%s", show)
	}
	// Injection guard on line_range.
	if _, err := runToolErr(t, GitTool{}, dir, map[string]any{"op": "blame", "path": "a.go", "line_range": "1 --output=/tmp/x"}); err == nil {
		t.Errorf("expected line_range injection rejection")
	}
}

func TestAffectedTests(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := goModule(t)
	mkFile(t, dir, "p.go", "package p\n\nfunc Target() {}\n")
	mkFile(t, dir, "p_test.go", "package p\n\nimport \"testing\"\n\nfunc TestTarget(t *testing.T) { Target() }\n")
	gitCommit(t, dir)
	// Modify the source file so it shows as changed.
	mkFile(t, dir, "p.go", "package p\n\nfunc Target() { _ = 1 }\n")

	out := runTool(t, AffectedTestsTool{}, dir, map[string]any{})
	if !strings.Contains(out, "TestTarget") {
		t.Errorf("expected TestTarget impacted:\n%s", out)
	}
}

func TestRenameSymbol(t *testing.T) {
	dir := goModule(t)
	mkFile(t, dir, "a.go", "package p\n\nfunc Old() {}\n\nfunc Caller() { Old() }\n")

	// Dry run does not write.
	dry := runTool(t, RenameSymbolTool{}, dir, map[string]any{"old_name": "Old", "new_name": "Renamed"})
	if !strings.Contains(dry, "DRY RUN") {
		t.Errorf("expected dry run:\n%s", dry)
	}
	if strings.Contains(readF(t, dir, "a.go"), "Renamed") {
		t.Errorf("dry run must not write")
	}

	// Apply.
	out := runTool(t, RenameSymbolTool{}, dir, map[string]any{"old_name": "Old", "new_name": "Renamed", "dry_run": false})
	if !strings.Contains(out, "applied") {
		t.Errorf("expected applied:\n%s", out)
	}
	got := readF(t, dir, "a.go")
	if !strings.Contains(got, "func Renamed()") || !strings.Contains(got, "Renamed()") {
		t.Errorf("rename not applied:\n%s", got)
	}
	if strings.Contains(got, "Old") {
		t.Errorf("old name lingers:\n%s", got)
	}
}

func TestStructuralReplace(t *testing.T) {
	dir := goModule(t)
	mkFile(t, dir, "a.go", "package p\n\nfunc f(a, b, c, d int) {\n\tg(a, b)\n\tg(c, d)\n}\n\nfunc g(x, y int) {}\n\nfunc h(x, y int) {}\n")

	// Find.
	find := runTool(t, StructuralReplaceTool{}, dir, map[string]any{"pattern": "g(x, y)", "vars": []string{"x", "y"}})
	if !strings.Contains(find, "2 match") {
		t.Errorf("expected 2 matches:\n%s", find)
	}

	// Rewrite (apply): g(x,y) -> h(y,x).
	out := runTool(t, StructuralReplaceTool{}, dir, map[string]any{"pattern": "g(x, y)", "rewrite": "h(y, x)", "vars": []string{"x", "y"}, "dry_run": false})
	if !strings.Contains(out, "APPLIED") {
		t.Errorf("expected applied:\n%s", out)
	}
	got := readF(t, dir, "a.go")
	if !strings.Contains(got, "h(b, a)") || !strings.Contains(got, "h(d, c)") {
		t.Errorf("structural rewrite not applied:\n%s", got)
	}
}

// TestStructuralReplaceCapBoundsWrites locks the fix that the rewrite applies EXACTLY
// the (capped) matches the report shows — never more (review-confirmed high bug).
func TestStructuralReplaceCapBoundsWrites(t *testing.T) {
	dir := goModule(t)
	mkFile(t, dir, "a.go", "package p\n\nfunc old(int) {}\n\nfunc a() { old(1); old(2); old(3); old(4) }\n")
	out := runTool(t, StructuralReplaceTool{}, dir, map[string]any{
		"pattern": "old(x)", "rewrite": "neu(x)", "vars": []string{"x"}, "dry_run": false, "max_matches": 2,
	})
	if !strings.Contains(out, "2 match") {
		t.Errorf("expected report capped at 2:\n%s", out)
	}
	got := readF(t, dir, "a.go")
	if n := strings.Count(got, "neu("); n != 2 {
		t.Errorf("expected exactly 2 rewrites (report = writes), got %d:\n%s", n, got)
	}
	if !strings.Contains(got, "old(3)") || !strings.Contains(got, "old(4)") {
		t.Errorf("expected the 2 un-accepted originals to remain:\n%s", got)
	}
}

// TestStructuralReplaceSubstNoCorruption locks the single-pass substitution: a bound
// value that contains another var's name must not be re-scanned (review-confirmed high bug).
func TestStructuralReplaceSubstNoCorruption(t *testing.T) {
	// binds x="y", y="z"; rewrite "f(x, y)" must become "f(y, z)", NOT "f(z, z)".
	got := substituteRewrite("f(x, y)", map[string]string{"x": "y", "y": "z"})
	if got != "f(y, z)" {
		t.Errorf("substitution corrupted by re-scan: got %q want %q", got, "f(y, z)")
	}
}

// TestImportGraphIgnoresTestPackages locks the fix that external _test.go packages do
// not manufacture false cycles (review-confirmed high bug).
func TestImportGraphIgnoresTestPackages(t *testing.T) {
	dir := goModule(t)
	mkFile(t, dir, "a/a.go", "package a\n")
	mkFile(t, dir, "b/b.go", "package b\n\nimport _ \"testmod/a\"\n")
	// External test package for a that imports b → would create a FALSE a⇄b cycle if
	// test files were folded into the production graph.
	mkFile(t, dir, "a/a_ext_test.go", "package a_test\n\nimport _ \"testmod/b\"\n")
	out := runTool(t, ImportGraphTool{}, dir, map[string]any{"op": "cycles"})
	if !strings.Contains(out, "acyclic") {
		t.Errorf("expected acyclic (test files must be ignored):\n%s", out)
	}
}

// TestRenameAtomicRollback verifies applyRenameAtomic restores already-written files
// when a later write fails — no half-applied rename. It targets the helper directly
// with a read-only second directory to make the second write fail reliably.
func TestRenameAtomicRollback(t *testing.T) {
	dir := t.TempDir()
	mkFile(t, dir, "ok.txt", "ORIGINAL")
	mkFile(t, dir, "ro/locked.txt", "LOCKED")
	roDir := filepath.Join(dir, "ro")
	if err := os.Chmod(roDir, 0o555); err != nil { // remove write bit ⇒ temp-file create fails
		t.Skipf("cannot make dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) }) // let TempDir cleanup remove it

	changes := []renameChange{
		{rel: "ok.txt", count: 1, bytes: []byte("NEW")},
		{rel: "ro/locked.txt", count: 1, bytes: []byte("NEW2")},
	}
	err := applyRenameAtomic(dir, changes)
	if err == nil {
		t.Skip("write to a read-only dir unexpectedly succeeded (running as root?)")
	}
	if got := readF(t, dir, "ok.txt"); got != "ORIGINAL" {
		t.Errorf("ok.txt should be rolled back to ORIGINAL, got %q", got)
	}
}

// gitCommit initializes a repo in dir and commits everything, using explicit author/
// committer identity env so it never depends on host git config.
func gitCommit(t *testing.T, dir string) {
	t.Helper()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
