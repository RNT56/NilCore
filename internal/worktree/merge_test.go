package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// twoBranches makes a repo with a base commit and two branches diverged off it:
// dep1 adds (or sets) file1, dep2 adds (or sets) file2. Returns the repo path.
// Leaves HEAD detached on the base so both branch refs are stable.
func twoBranches(t *testing.T, file1, body1, file2, body2 string) string {
	t.Helper()
	repo := initRepo(t)
	baseSHA := strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD"))
	gitOut(t, repo, "checkout", "-q", "-b", "dep1", baseSHA)
	commitFile(t, repo, file1, body1, "dep1")
	gitOut(t, repo, "checkout", "-q", "-b", "dep2", baseSHA)
	commitFile(t, repo, file2, body2, "dep2")
	gitOut(t, repo, "checkout", "-q", baseSHA) // detach so dep1/dep2 are not checked out
	return repo
}

// Merge of two non-conflicting branches yields a worktree tree containing BOTH
// branches' files — the multi-dep re-base union (Phase 2).
func TestMergeNonConflicting(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	repo := twoBranches(t, "a.txt", "AAA\n", "b.txt", "BBB\n")

	wt, err := CreateFrom(ctx, repo, "rebase/x", "rb", "dep1")
	if err != nil {
		t.Fatalf("CreateFrom dep1: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	conflict, err := wt.Merge(ctx, "dep2", "merge dep2")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if conflict {
		t.Fatal("non-conflicting branches reported a conflict")
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(wt.Path(), f)); err != nil {
			t.Errorf("merged tree missing %s: %v", f, err)
		}
	}
	// The merge was committed (no lingering MERGE_HEAD, clean tree).
	if st := strings.TrimSpace(gitOut(t, wt.Path(), "status", "--porcelain")); st != "" {
		t.Errorf("worktree not clean after merge: %q", st)
	}
}

// Merge of two branches that touch the SAME file divergently is a CONFLICT: it
// reports conflict=true (no Go error), aborts cleanly, and leaves the worktree at
// the pre-merge state (no conflict markers, no MERGE_HEAD) — the graceful path the
// re-base relies on to fall back to base HEAD.
func TestMergeConflictAbortsClean(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	repo := twoBranches(t, "c.txt", "X\n", "c.txt", "Y\n")

	wt, err := CreateFrom(ctx, repo, "rebase/y", "rb2", "dep1")
	if err != nil {
		t.Fatalf("CreateFrom dep1: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	conflict, err := wt.Merge(ctx, "dep2", "merge dep2")
	if err != nil {
		t.Fatalf("a conflict must be conflict=true, not a Go error: %v", err)
	}
	if !conflict {
		t.Fatal("divergent same-file branches should conflict")
	}
	// Abort restored dep1's content and left a clean tree (no MERGE_HEAD / markers).
	if st := strings.TrimSpace(gitOut(t, wt.Path(), "status", "--porcelain")); st != "" {
		t.Errorf("worktree not clean after conflict abort: %q", st)
	}
	body, err := os.ReadFile(filepath.Join(wt.Path(), "c.txt"))
	if err != nil || strings.TrimSpace(string(body)) != "X" {
		t.Errorf("c.txt = %q (err=%v), want dep1's X (abort restored the pre-merge tip)", body, err)
	}
}
