package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WorkingDiff reports a worker's UNCOMMITTED work-in-progress: a tracked-file edit
// shows as a diff, a brand-new file shows in the untracked list, and a pristine
// worktree reports nothing — the consistent "what I've done so far" snapshot a
// subagent attaches when it asks the supervisor.
func TestWorkingDiffShowsUncommittedWork(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	repo := initRepo(t)
	commitFile(t, repo, "main.go", "package p\n\nfunc Run() {}\n", "seed")

	wt, err := CreateFrom(ctx, repo, "task/t1", "t1", "HEAD")
	if err != nil {
		t.Fatalf("CreateFrom: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	// Pristine: no work yet.
	if d, err := wt.WorkingDiff(ctx, 0); err != nil || d != "" {
		t.Fatalf("pristine worktree WorkingDiff = %q (err %v), want empty", d, err)
	}

	// Modify a tracked file (uncommitted) and add a brand-new untracked file.
	if err := os.WriteFile(filepath.Join(wt.Path(), "main.go"), []byte("package p\n\nfunc Run() { println(\"hi\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path(), "helper.go"), []byte("package p\n\nfunc help() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := wt.WorkingDiff(ctx, 0)
	if err != nil {
		t.Fatalf("WorkingDiff: %v", err)
	}
	if !strings.Contains(d, "main.go") || !strings.Contains(d, "println") {
		t.Errorf("WorkingDiff should show the uncommitted edit to main.go:\n%s", d)
	}
	if !strings.Contains(d, "helper.go") || !strings.Contains(d, "untracked") {
		t.Errorf("WorkingDiff should list the new untracked helper.go:\n%s", d)
	}
	// Read-only: it must NOT have staged or committed anything.
	if st := strings.TrimSpace(gitOut(t, wt.Path(), "diff", "--cached", "--name-only")); st != "" {
		t.Errorf("WorkingDiff must not stage anything; index shows: %q", st)
	}
}
