package tools

// affectedtests.go — activates the otherwise-dark internal/codeintel/impact package
// as a model-facing planning input: "which tests should I re-run for what I just
// changed?" It reads the changed files (git status, or an explicit list), treats
// every symbol they define as changed (a safe OVER-approximation — never under-runs),
// unions impact.AffectedTests over the call graph, and re-joins the bare test names
// to their files so the output is actually usable with `go test -run`.
//
// ADVISORY ONLY (I2): this is a fast-path hint to shrink the inner loop, never a
// substitute for the full verifier suite that governs "done". Graph nodes carry a
// QUALIFIED id (file+recv+name), but a call site names its callee only by BARE name,
// so impact.AffectedTests resolves the changed bare name to every same-named
// definition and walks their callers — it over-approximates on cross-file/package name
// collisions deliberately, since running an extra test is safe and missing one is not.

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"nilcore/internal/codeintel/ast"
	"nilcore/internal/codeintel/impact"
)

// AffectedTestsTool reports the tests impacted by the current (or given) changes.
type AffectedTestsTool struct{}

func (AffectedTestsTool) Name() string { return "affected_tests" }
func (AffectedTestsTool) Description() string {
	return "Read-only: list the tests worth re-running for the current changes (or an explicit 'paths' " +
		"list), via the call graph. ADVISORY — a fast-path hint to shrink the loop; the full verifier suite " +
		"still governs done. Over-approximates safely (never under-runs). No writes, no execution of tests."
}
func (AffectedTestsTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"paths":{"type":"array","items":{"type":"string"}}},"required":[]}`)
}

func (AffectedTestsTool) Run(ctx context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Paths []string `json:"paths"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("bad input: %w", err)
		}
	}

	changed := in.Paths
	if len(changed) == 0 {
		changed = changedFiles(ctx, workdir)
	}
	// Keep only files the AST layer can parse (others contribute no symbols anyway).
	supported := map[string]bool{}
	for _, e := range ast.SupportedExtensions() {
		supported[strings.ToLower(e)] = true
	}
	var srcChanged []string
	for _, f := range changed {
		if supported[strings.ToLower(extLower(f))] {
			srcChanged = append(srcChanged, f)
		}
	}
	if len(srcChanged) == 0 {
		return "affected_tests: no changed source files detected — run the full suite (`make verify`).", nil
	}

	// The set of changed symbol names = every symbol defined in a changed file.
	changedSyms := map[string]bool{}
	for _, rel := range srcChanged {
		abs, err := safePath(workdir, rel)
		if err != nil {
			continue
		}
		syms, serr := ast.Symbols(abs)
		if serr != nil {
			continue
		}
		for _, s := range syms {
			changedSyms[s.Name] = true
		}
	}

	g, _, err := buildGraph(ctx, workdir)
	if err != nil {
		return "", err
	}
	defer g.Close()

	testNames := map[string]bool{}
	for name := range changedSyms {
		tests, terr := impact.AffectedTests(ctx, g, name)
		if terr != nil {
			return "", terr
		}
		for _, t := range tests {
			testNames[t] = true
		}
		// A changed symbol that is itself a test counts too.
		if strings.HasPrefix(name, "Test") {
			testNames[name] = true
		}
	}

	if len(testNames) == 0 {
		return "affected_tests: no impacted tests found via the call graph — still run the full suite to be safe.", nil
	}

	// Re-join test names to their files (impact.AffectedTests returns BARE test names;
	// `go test -run` needs the package), via a worktree symbol index.
	allSyms, _, _ := worktreeSymbols(ctx, workdir)
	fileOf := map[string][]string{}
	for _, s := range allSyms {
		if testNames[s.Name] {
			fileOf[s.Name] = append(fileOf[s.Name], s.Rel)
		}
	}

	names := make([]string, 0, len(testNames))
	for t := range testNames {
		names = append(names, t)
	}
	sort.Strings(names)

	var sb strings.Builder
	fmt.Fprintf(&sb, "affected_tests: %d impacted test(s) for %d changed file(s) (ADVISORY — full suite still governs):\n", len(names), len(srcChanged))
	for _, t := range names {
		files := fileOf[t]
		sort.Strings(files)
		if len(files) == 0 {
			fmt.Fprintf(&sb, "- %s\n", t)
		} else {
			fmt.Fprintf(&sb, "- %s  (%s)\n", t, strings.Join(dedup(files), ", "))
		}
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// changedFiles returns the worktree-relative paths git reports as changed (staged,
// unstaged, and untracked), best-effort. A non-repo or git error yields nil (the
// caller then advises a full run). git runs with the shared hardening clamp.
func changedFiles(ctx context.Context, workdir string) []string {
	cmd := exec.CommandContext(ctx, "git", append(HardenArgs(), "status", "--porcelain")...)
	cmd.Dir = workdir
	cmd.Env = HardenedEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		// Renames appear as "old -> new"; take the new path.
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		path = strings.Trim(path, `"`)
		if path != "" {
			files = append(files, path)
		}
	}
	return files
}

// extLower returns the lower-cased file extension including the dot.
func extLower(path string) string {
	i := strings.LastIndexByte(path, '.')
	if i < 0 {
		return ""
	}
	return strings.ToLower(path[i:])
}

// dedup returns s with consecutive-duplicate-free, order-preserving entries (input
// is already sorted by callers).
func dedup(s []string) []string {
	out := s[:0:0]
	seen := map[string]bool{}
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
